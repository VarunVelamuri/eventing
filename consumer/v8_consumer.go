package consumer

import (
	"fmt"
	"math/rand"
	"net"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/couchbase/eventing/common"
	"github.com/couchbase/eventing/util"
	cblib "github.com/couchbase/go-couchbase"
	"github.com/couchbase/indexing/secondary/dcp"
	mcd "github.com/couchbase/indexing/secondary/dcp/transport"
	"github.com/couchbase/indexing/secondary/logging"
)

func New(p common.EventingProducer, app *common.AppConfig, vbnos []uint16, bucket, tcpPort string, workerId int) *Consumer {
	var b *couchbase.Bucket
	consumer := &Consumer{
		app:                  app,
		bucket:               bucket,
		cbBucket:             b,
		gracefulShutdownChan: make(chan bool, 1),
		producer:             p,
		signalConnectedCh:    make(chan bool),
		statsTicker:          time.NewTicker(1000 * time.Millisecond),
		tcpPort:              tcpPort,
		vbFlogChan:           make(chan *vbFlogEntry),
		vbnos:                vbnos,
		vbProcessingStats:    newVbProcessingStats(),
		workerName:           fmt.Sprintf("worker_%s_%d", app.AppName, workerId),
	}
	return consumer
}

func (c *Consumer) Serve() {
	c.stopConsumerCh = make(chan bool, 1)
	c.stopCheckpointingCh = make(chan bool)
	c.stopVbTakeoverCh = make(chan bool)

	c.dcpMessagesProcessed = make(map[mcd.CommandCode]int)
	c.v8WorkerMessagesProcessed = make(map[string]int)

	c.initCBBucketConnHandle()

	dcpConfig := map[string]interface{}{
		"genChanSize":    DcpGenChanSize,
		"dataChanSize":   DcpDataChanSize,
		"numConnections": DcpNumConnections,
	}

	util.Retry(util.NewFixedBackoff(time.Second), commonConnectBucketOpCallback, c, &c.cbBucket)

	var flogs couchbase.FailoverLog
	util.Retry(util.NewFixedBackoff(BucketOpRetryInterval), getFailoverLogOpCallback, c, &flogs, dcpConfig)

	logging.Infof("V8CR[%s:%s:%s:%d] vbnos len: %d vbnos dump: %#v",
		c.app.AppName, c.workerName, c.tcpPort, c.osPid, len(c.vbnos), c.vbnos)

	logging.Infof("V8CR[%s:%s:%s:%d] Spawning worker corresponding to producer",
		c.app.AppName, c.workerName, c.tcpPort, c.osPid)

	c.cmd = exec.Command("client", c.app.AppName, c.tcpPort,
		time.Now().UTC().Format("2006-01-02T15:04:05.000000000-0700"))

	err := c.cmd.Start()
	if err != nil {
		logging.Errorf("V8CR[%s:%s:%s:%d] Failed to spawn worker, err: %v",
			c.app.AppName, c.workerName, c.tcpPort, c.osPid, err)
	} else {
		c.osPid = c.cmd.Process.Pid
		logging.Infof("V8CR[%s:%s:%s:%d] c++ worker launched",
			c.app.AppName, c.workerName, c.tcpPort, c.osPid)
	}

	rand.Seed(time.Now().UnixNano())
	feedName := couchbase.DcpFeedName("eventing:" + c.workerName + "_" + strconv.Itoa(rand.Int()))
	util.Retry(util.NewFixedBackoff(BucketOpRetryInterval), startDCPFeedOpCallback, c, feedName, dcpConfig)

	go c.startDcp(dcpConfig, flogs)

	go func(c *Consumer) {
		c.cmd.Wait()
	}(c)

	// Wait for net.Conn to be initialised
	<-c.signalConnectedCh

	c.sendInitV8Worker(c.app.AppName)
	res := c.readMessage()
	logging.Infof("V8CR[%s:%s:%s:%d] Response from worker for init call: %s",
		c.app.AppName, c.workerName, c.tcpPort, c.osPid, res.response)

	c.sendLoadV8Worker(c.app.AppCode)
	res = c.readMessage()
	logging.Infof("V8CR[%s:%s:%s:%d] Response from worker for app load call: %s",
		c.app.AppName, c.workerName, c.tcpPort, c.osPid, res.response)

	go c.doLastSeqNoCheckpoint()
	go c.doVbucketTakeover()

	c.doDCPEventProcess()
}

func (c *Consumer) Stop() {
	logging.Infof("V8CR[%s:%s:%s:%d] Gracefully shutting down consumer routine\n",
		c.app.AppName, c.workerName, c.tcpPort, c.osPid)

	c.producer.CleanupDeadConsumer(c)

	c.cmd.Process.Kill()

	c.statsTicker.Stop()
	c.stopCheckpointingCh <- true
	c.stopVbTakeoverCh <- true
	c.gracefulShutdownChan <- true
	c.dcpFeed.Close()
}

// Implement fmt.Stringer interface to allow better debugging
// if C++ V8 worker crashes
func (c *Consumer) String() string {
	return fmt.Sprintf("consumer => app: %s tcpPort: %s ospid: %d"+
		" dcpEventProcessed: %s v8EventProcessed: %s", c.app.AppName, c.tcpPort,
		c.osPid, util.SprintDCPCounts(c.dcpMessagesProcessed),
		util.SprintV8Counts(c.v8WorkerMessagesProcessed))
}

func (c *Consumer) SignalConnected() {
	c.signalConnectedCh <- true
}

func (c *Consumer) SetConnHandle(conn net.Conn) {
	c.conn = conn
}

func (c *Consumer) HostPortAddr() string {
	hostPortAddr := (*string)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&c.hostPortAddr))))
	if hostPortAddr != nil {
		return *hostPortAddr
	} else {
		return ""
	}
}

func (c *Consumer) ConsumerName() string {
	c.RLock()
	defer c.RUnlock()
	return c.workerName
}

func (c *Consumer) initCBBucketConnHandle() {
	config := c.app.Depcfg.(map[string]interface{})
	metadataBucket := config["metadata_bucket"].(string)
	connStr := fmt.Sprintf("http://127.0.0.1:" + c.producer.GetNsServerPort())

	var conn cblib.Client
	var pool cblib.Pool

	util.Retry(util.NewFixedBackoff(BucketOpRetryInterval), connectBucketOpCallback, c, &conn, connStr)

	util.Retry(util.NewFixedBackoff(BucketOpRetryInterval), poolGetBucketOpCallback, c, &conn, &pool, "default")

	util.Retry(util.NewFixedBackoff(BucketOpRetryInterval), cbGetBucketOpCallback, c, &pool, metadataBucket)
}