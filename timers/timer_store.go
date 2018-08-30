package timers

/* This module returns only common.ErrRetryTimeout error */

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/eventing/logging"
	"github.com/couchbase/gocb"
	"golang.org/x/crypto/ripemd160"
)

// Constants
const (
	Resolution  = int64(7) // seconds
	init_seq    = int64(128)
	dict        = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789*&"
	encode_base = 36
)

// Globals
var (
	stores = newStores()
)

type storeMap struct {
	lock    sync.RWMutex
	entries map[string]*TimerStore
}

type AlarmRecord struct {
	AlarmDue   int64  `json:"due"`
	ContextRef string `json:"cxr"`
}

type ContextRecord struct {
	Context  interface{} `json:"ctx"`
	AlarmRef string      `json:"alr"`
}

type TimerEntry struct {
	AlarmRecord
	ContextRecord

	alarmSeq int64
	ctxCas   gocb.Cas
	topCas   gocb.Cas
}

type rowIter struct {
	start   int64
	stop    int64
	current int64
}

type colIter struct {
	stop    int64
	current int64
	topCas  gocb.Cas
}

type Span struct {
	Start int64 `json:"sta"`
	Stop  int64 `json:"stp"`
}

type storeSpan struct {
	Span
	empty   bool
	dirty   bool
	spanCas gocb.Cas
	lock    sync.Mutex
}

type TimerStore struct {
	connstr string
	bucket  string
	uid     string
	partn   int
	log     string
	span    storeSpan
	stats   timerStats
}

type TimerIter struct {
	store *TimerStore
	row   rowIter
	col   *colIter
	entry *TimerEntry
}

type timerStats struct {
	cancelCounter               uint64 `json:"meta_cancel"`
	cancelSuccessCounter        uint64 `json:"meta_cancel_success"`
	delCounter                  uint64 `json:"meta_del"`
	delSuccessCounter           uint64 `json:"meta_del_success"`
	setCounter                  uint64 `json:"meta_set"`
	setSuccessCounter           uint64 `json:"meta_set_success"`
	timerInPastCounter          uint64 `json:"meta_timer_in_past"`
	timerInFutureFiredCounter   uint64 `json:"meta_timer_in_future_fired"`
	alarmMissingCounter         uint64 `json:"meta_alarm_missing"`
	contextMissingCounter       uint64 `json:"meta_context_missing"`
	cancelAlarmMissingCounter   uint64 `json:"meta_cancel_alarm_missing"`
	cancelContextMissingCounter uint64 `json:"meta_cancel_context_missing"`
	scanDueCounter              uint64 `json:"meta_scan_due"`
	scanRowCounter              uint64 `json:"meta_scan_row"`
	scanRowLookupCounter        uint64 `json:"meta_scan_row_lookup"`
	scanColumnCounter           uint64 `json:"meta_scan_column"`
	scanColumnLookupCounter     uint64 `json:"meta_scan_column_lookup"`
	syncSpanCounter             uint64 `json:"meta_sync_span"`
	externalSpanChangeCounter   uint64 `json:"meta_external_span_change"`
	spanStartChangeCounter      uint64 `json:"meta_span_start_change"`
	spanStopChangeCounter       uint64 `json:"meta_span_stop_change"`
	spanCasMismatchCounter      uint64 `json:"meta_span_cas_mismatch"`
}

func Create(uid string, partn int, connstr string, bucket string) error {
	stores.lock.Lock()
	defer stores.lock.Unlock()

	_, found := stores.entries[mapLocator(uid, partn)]
	if found {
		logging.Warnf("Asked to create store %v:%v which exists. Reusing", uid, partn)
		return nil
	}
	store, err := newTimerStore(uid, partn, connstr, bucket)
	if err != nil {
		return err
	}
	stores.entries[mapLocator(uid, partn)] = store
	return nil
}

func Fetch(uid string, partn int) (store *TimerStore, found bool) {
	stores.lock.RLock()
	defer stores.lock.RUnlock()
	store, found = stores.entries[mapLocator(uid, partn)]
	if !found {
		logging.Infof("Store not defined: " + mapLocator(uid, partn))
		return nil, false
	}
	return
}

func (r *TimerStore) Free() {
	stores.lock.Lock()
	delete(stores.entries, mapLocator(r.uid, r.partn))
	stores.lock.Unlock()
	r.syncSpan()
}

func (r *TimerStore) Set(due int64, ref string, context interface{}) error {
	now := time.Now().Unix()
	atomic.AddUint64(&r.stats.setCounter, 1)

	if due-now <= Resolution {
		atomic.AddUint64(&r.stats.timerInPastCounter, 1)
		logging.Tracef("%v Moving too close/past timer to next period: %v context %ru", r.log, formatInt(due), context)
		due = now + Resolution
	}
	due = roundUp(due)

	kv := Pool(r.connstr)
	pos := r.kvLocatorRoot(due)
	seq, _, err := kv.MustCounter(r.bucket, pos, 1, init_seq, 0)
	if err != nil {
		return err
	}

	akey := r.kvLocatorAlarm(due, seq)
	ckey := r.kvLocatorContext(ref)

	arecord := AlarmRecord{AlarmDue: due, ContextRef: ckey}
	_, err = kv.MustUpsert(r.bucket, akey, arecord, 0)
	if err != nil {
		return err
	}

	crecord := ContextRecord{Context: context, AlarmRef: akey}
	_, err = kv.MustUpsert(r.bucket, ckey, crecord, 0)
	if err != nil {
		return err
	}

	logging.Tracef("%v Creating timer at %v seq %v with ref %ru and context %ru", r.log, seq, formatInt(due), ref, context)
	r.expandSpan(due)
	atomic.AddUint64(&r.stats.setSuccessCounter, 1)
	return nil
}

func (r *TimerStore) Delete(entry *TimerEntry) error {
	logging.Tracef("%v Deleting timer %+v", r.log, entry)
	atomic.AddUint64(&r.stats.delCounter, 1)
	kv := Pool(r.connstr)

	_, absent, _, err := kv.MustRemove(r.bucket, entry.AlarmRef, 0)
	if err != nil {
		return err
	}
	if absent {
		logging.Tracef("%v Timer %v seq %v is missing alarm in del: %ru", r.log, entry.AlarmDue, entry.alarmSeq, *entry)
	}

	_, absent, mismatch, err := kv.MustRemove(r.bucket, entry.ContextRef, entry.ctxCas)
	if err != nil {
		return err
	}
	if absent {
		atomic.AddUint64(&r.stats.contextMissingCounter, 1)
	}

	if mismatch {
		logging.Tracef("%v Timer %v seq %v was either cancelled or overridden after it fired: %ru", r.log, entry.AlarmDue, entry.alarmSeq, *entry)
		return nil
	}

	if entry.topCas == 0 {
		return nil
	}

	pos := r.kvLocatorRoot(entry.AlarmDue)
	logging.Debugf("%v Removing last entry, so removing counter %+v", r.log, pos)

	_, absent, mismatch, err = kv.MustRemove(r.bucket, pos, entry.topCas)
	if err != nil {
		return err
	}
	if absent || mismatch {
		atomic.AddUint64(&r.stats.alarmMissingCounter, 1)
		logging.Tracef("%v Concurrency on %v absent:%v mismatch:%v", r.log, pos, absent, mismatch)
	}

	r.shrinkSpan(entry.AlarmDue)
	atomic.AddUint64(&r.stats.delSuccessCounter, 1)
	return nil
}

func (r *TimerStore) Cancel(ref string) error {
	atomic.AddUint64(&r.stats.cancelCounter, 1)
	logging.Tracef("%v Cancelling timer ref %ru", r.log, ref)

	kv := Pool(r.connstr)
	cref := r.kvLocatorContext(ref)

	crecord := ContextRecord{}
	_, absent, err := kv.MustGet(r.bucket, cref, &crecord)
	if err != nil {
		return nil
	}
	if absent {
		atomic.AddUint64(&r.stats.cancelContextMissingCounter, 1)
		logging.Tracef("%v Timer asked to cancel %ru cref %v does not exist", r.log, ref, cref)
		return nil
	}

	_, absent, _, err = kv.MustRemove(r.bucket, crecord.AlarmRef, 0)
	if err != nil {
		return nil
	}
	if absent {
		atomic.AddUint64(&r.stats.cancelAlarmMissingCounter, 1)
		logging.Tracef("%v Timer asked to cancel %ru alarmref %v does not exist", r.log, ref, crecord.AlarmRef)
		return nil
	}

	_, absent, _, err = kv.MustRemove(r.bucket, cref, 0)
	if err != nil {
		return nil
	}
	if absent {
		logging.Tracef("%v Timer asked to cancel %ru cref %v does not exist", r.log, ref, cref)
	}

	// TODO: if all items were canceled, need to remove top

	atomic.AddUint64(&r.stats.cancelSuccessCounter, 1)
	return nil
}

func (r *TimerStore) Partition() int {
	return r.partn
}

func (r *TimerStore) ScanDue() *TimerIter {
	span := r.readSpan()
	now := roundDown(time.Now().Unix())

	atomic.AddUint64(&r.stats.scanDueCounter, 1)
	if span.Start > now {
		return nil
	}

	stop := now
	if stop > span.Stop {
		stop = span.Stop
	}

	iter := TimerIter{
		store: r,
		entry: nil,
		row: rowIter{
			start:   span.Start,
			current: span.Start,
			stop:    stop,
		},
		col: nil,
	}

	logging.Tracef("%v Created iterator: %+v", r.log, iter)
	return &iter
}

func (r *TimerIter) ScanNext() (*TimerEntry, error) {
	if r == nil {
		return nil, nil
	}

	for {
		logging.Tracef("Scan next iterator: %+v", r)
		found, err := r.nextColumn()
		if err != nil {
			return nil, err
		}
		if found {
			if r.entry.AlarmDue > time.Now().Unix() {
				atomic.AddUint64(&r.store.stats.timerInFutureFiredCounter, 1)
			}
			return r.entry, nil
		}
		found, err = r.nextRow()
		if !found || err != nil {
			return nil, err
		}
	}
}

func (r *TimerIter) nextRow() (bool, error) {
	atomic.AddUint64(&r.store.stats.scanRowCounter, 1)
	logging.Tracef("%v Looking for row after %+v", r.store.log, r.row)
	kv := Pool(r.store.connstr)

	r.col = nil
	r.entry = nil

	col := colIter{current: init_seq, topCas: 0}
	for r.row.current < r.row.stop {
		r.row.current += Resolution

		pos := r.store.kvLocatorRoot(r.row.current)
		atomic.AddUint64(&r.store.stats.scanRowLookupCounter, 1)
		cas, absent, err := kv.MustGet(r.store.bucket, pos, &col.stop)
		if err != nil {
			return false, err
		}
		if !absent {
			col.topCas = cas
			r.col = &col
			logging.Tracef("%v Found row %+v", r.store.log, r.row)
			return true, nil
		}
	}
	logging.Tracef("%v Found no rows looking until %v", r.store.log, r.row.stop)
	r.store.shrinkSpan(r.row.stop - Resolution)
	return false, nil
}

func (r *TimerIter) nextColumn() (bool, error) {
	atomic.AddUint64(&r.store.stats.scanColumnCounter, 1)
	logging.Tracef("%v Looking for column after %+v in row %+v", r.store.log, r.col, r.row)
	r.entry = nil

	if r.col == nil {
		return false, nil
	}

	kv := Pool(r.store.connstr)
	alarm := AlarmRecord{}
	context := ContextRecord{}

	for r.col.current <= r.col.stop {
		current := r.col.current
		r.col.current++

		key := r.store.kvLocatorAlarm(r.row.current, current)

		atomic.AddUint64(&r.store.stats.scanColumnLookupCounter, 1)
		_, absent, err := kv.MustGet(r.store.bucket, key, &alarm)
		if err != nil {
			return false, err
		}
		if absent {
			logging.Debugf("%v Skipping missing entry in chain at %v", r.store.log, key)
			continue
		}

		atomic.AddUint64(&r.store.stats.scanColumnLookupCounter, 1)
		cas, absent, err := kv.MustGet(r.store.bucket, alarm.ContextRef, &context)
		if err != nil {
			return false, err
		}
		if absent || context.AlarmRef != key {
			// Alarm canceled if absent, or superseded if AlarmRef != key
			logging.Debugf("%v Alarm canceled or superseded %v by context %ru", r.store.log, alarm, context)
			continue
		}

		r.entry = &TimerEntry{AlarmRecord: alarm, ContextRecord: context, alarmSeq: current, topCas: 0, ctxCas: cas}
		if current == r.col.stop {
			r.entry.topCas = r.col.topCas
		}
		logging.Tracef("%v Scan returning timer %+v", r.store.log, r.entry)
		return true, nil

	}

	logging.Tracef("%v Column scan finished for %+v", r.store.log, r)
	return false, nil
}

func (r *TimerStore) readSpan() Span {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	return r.span.Span
}

func (r *TimerStore) expandSpan(point int64) {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	if r.span.Start > point {
		r.span.Start = point
		r.span.dirty = true
	}
	if r.span.Stop < point {
		r.span.Stop = point
		r.span.dirty = true
	}
}

func (r *TimerStore) shrinkSpan(start int64) {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	if r.span.Start < start {
		r.span.Start = start
		r.span.dirty = true
	}
}

func (r *TimerStore) syncSpan() error {
	atomic.AddUint64(&r.stats.syncSpanCounter, 1)
	logging.Tracef("%v syncSpan called", r.log)

	r.span.lock.Lock()
	defer r.span.lock.Unlock()

	if !r.span.dirty && !r.span.empty {
		return nil
	}

	r.span.dirty = false
	kv := Pool(r.connstr)
	pos := r.kvLocatorSpan()
	extspan := Span{}

	rcas, absent, err := kv.MustGet(r.bucket, pos, &extspan)
	if err != nil {
		return err
	}

	// Initial setup cases
	switch {

	// new, not on disk, not on node
	case absent && r.span.empty:
		now := time.Now().Unix()
		r.span.Span = Span{Start: roundDown(now), Stop: roundUp(now)}
		wcas, mismatch, err := kv.MustInsert(r.bucket, pos, r.span.Span, 0)
		if err != nil || mismatch {
			logging.Tracef("%v Error initializing span %+v: mismatch=%v err=%v", r.log, r.span, mismatch, err)
			return err
		}
		r.span.spanCas = wcas
		r.span.empty = false
		logging.Tracef("%v Span initialized as %+v", r.log, r.span)
		return nil

	// new, not persisted, but we have data locally
	case absent && !r.span.empty:
		wcas, mismatch, err := kv.MustInsert(r.bucket, pos, r.span.Span, 0)
		if err != nil || mismatch {
			logging.Tracef("%v Error initializing span %+v: mismatch=%v err=%v", r.log, r.span, mismatch, err)
			return err
		}
		r.span.spanCas = wcas
		logging.Tracef("%v Span created as %+v", r.log, r.span)
		return nil

	// we have no data, but some data has been persisted earlier
	case r.span.empty:
		r.span.empty = false
		r.span.Span = extspan
		r.span.spanCas = rcas
		logging.Tracef("%v Span read and initialized to %+v", r.log, r.span)
		return nil
	}

	// Happy path cases
	switch {

	// nothing has moved, either locally or in persisted version
	case r.span.spanCas == rcas && r.span.Span == extspan:
		logging.Tracef("%v Span no changes %+v", r.log, r.span)
		return nil

	// only internal changes, no conflict with persisted version
	case r.span.spanCas == rcas:
		logging.Tracef("%v Writing span no conflict %+v", r.log, r.span)
		wcas, absent, mismatch, err := kv.MustReplace(r.bucket, pos, r.span.Span, rcas, 0)
		if err != nil || absent || mismatch {
			logging.Tracef("%v Overwriting span %+v failed: absent=%v mismatch=%v err=%v", r.log, r.span, absent, mismatch, err)
			return err
		}
		r.span.spanCas = wcas
		return nil
	}

	// Merge conflict
	atomic.AddUint64(&r.stats.spanCasMismatchCounter, 1)
	if r.span.Start > extspan.Start {
		logging.Tracef("%v Span conflict external write, moving Start: span=%+v extspan=%+v", r.span, extspan)
		atomic.AddUint64(&r.stats.spanStartChangeCounter, 1)
		r.span.Start = extspan.Start
	}
	if r.span.Stop < extspan.Stop {
		logging.Tracef("%v Span conflict external write, moving Stop: span=%+v extspan=%+v", r.span, extspan)
		atomic.AddUint64(&r.stats.spanStopChangeCounter, 1)
		r.span.Stop = extspan.Stop
	}
	wcas, absent, mismatch, err := kv.MustReplace(r.bucket, pos, r.span.Span, rcas, 0)
	if err != nil || absent || mismatch {
		logging.Tracef("%v Overwriting span %+v failed: absent=%v mismatch=%v err=%v", r.log, r.span, absent, mismatch, err)
		return err
	}
	r.span.spanCas = wcas
	logging.Tracef("%v Span was merged and saved successfully: %+v", r.log, r.span)
	return nil
}

func (r *storeMap) syncRoutine() {
	for {
		dirty := make([]*TimerStore, 0)
		r.lock.RLock()
		for _, store := range r.entries {
			if store.span.dirty {
				dirty = append(dirty, store)
			}
		}
		r.lock.RUnlock()
		for _, store := range dirty {
			store.syncSpan()
		}
		time.Sleep(time.Duration(Resolution) * time.Second)
	}
}

func newTimerStore(uid string, partn int, connstr string, bucket string) (*TimerStore, error) {
	timerstore := TimerStore{
		connstr: connstr,
		bucket:  bucket,
		uid:     uid,
		partn:   partn,
		log:     fmt.Sprintf("timerstore:%v:%v", uid, partn),
		span:    storeSpan{empty: true, dirty: false},
	}

	err := timerstore.syncSpan()
	if err != nil {
		return nil, err
	}

	logging.Tracef("%v Initialized timerdata store", timerstore.log)
	return &timerstore, nil
}

func hash(val string) string {
	ripe := ripemd160.New()
	ripe.Write([]byte(val))
	sum := ripe.Sum(nil)
	hash := make([]byte, 27, 27)
	char := byte(0)
	for pos := 0; pos < 160; pos++ {
		bit := (sum[pos/8] >> uint(pos%8)) & 1
		char = char<<1 | bit
		if pos%6 == 5 {
			hash[pos/6] = dict[char]
			char = 0
		}
	}
	hash[26] = dict[char]
	return string(hash)
}

func (r *TimerStore) kvLocatorRoot(due int64) string {
	return fmt.Sprintf("%v:tm:%v:rt:%v", r.uid, r.partn, formatInt(due))
}

func (r *TimerStore) kvLocatorAlarm(due int64, seq int64) string {
	return fmt.Sprintf("%v:tm:%v:al:%v:%v", r.uid, r.partn, formatInt(due), seq)
}

func (r *TimerStore) kvLocatorContext(ref string) string {
	return fmt.Sprintf("%v:tm:%v:cx:%v", r.uid, r.partn, hash(ref))
}

func (r *TimerStore) kvLocatorSpan() string {
	return fmt.Sprintf("%v:tm:%v:sp", r.uid, r.partn)
}

func (r *TimerStore) Stats() map[string]uint64 {
	stats := make(map[string]uint64)
	t := reflect.TypeOf(r.stats)
	v := reflect.ValueOf(r.stats)
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("json")
		count := v.Field(i).Uint()
		stats[name] = count
	}
	return stats
}

func mapLocator(uid string, partn int) string {
	return fmt.Sprintf("%v:%v", uid, partn)
}

func (r *ContextRecord) String() string {
	format := logging.RedactFormat("ContextRecord[AlarmRef=%v, Context=%ru]")
	return fmt.Sprintf(format, r.AlarmRef, r.Context)
}

func formatInt(tm int64) string {
	return strconv.FormatInt(tm, encode_base)
}

func newStores() *storeMap {
	smap := &storeMap{
		entries: make(map[string]*TimerStore),
		lock:    sync.RWMutex{},
	}
	go smap.syncRoutine()
	return smap
}

func roundUp(val int64) int64 {
	q := val / Resolution
	r := val % Resolution
	if r > 0 {
		q++
	}
	return q * Resolution
}

func roundDown(val int64) int64 {
	q := val / Resolution
	return q * Resolution
}
