// +build all rebalance eventing_reb_wh

package eventing

import (
	"testing"
	"time"
)

func TestEventingRebNoKVOpsWithoutHandlerOneByOne(t *testing.T) {
	time.Sleep(5 * time.Second)

	addAllNodesOneByOne("eventing")
	removeAllNodesOneByOne()
}

func TestEventingRebNoKVOpsWithoutHandlerAllAtOnce(t *testing.T) {
	time.Sleep(5 * time.Second)

	addAllNodesAtOnce("eventing")
	removeAllNodesAtOnce()
}

func TestEventingRebKVOpsWithoutHandlerOneByOne(t *testing.T) {
	time.Sleep(5 * time.Second)

	rl := &rateLimit{
		limit:   true,
		opsPSec: rlOpsPSec,
		count:   rlItemCount,
		stopCh:  make(chan struct{}, 1),
		loop:    true,
	}

	go pumpBucketOps(opsType{count: rlItemCount}, rl)

	addAllNodesOneByOne("eventing")
	removeAllNodesOneByOne()

	rl.stopCh <- struct{}{}
}

func TestEventingRebKVOpsWithoutHandlerAllAtOnce(t *testing.T) {
	time.Sleep(5 * time.Second)

	rl := &rateLimit{
		limit:   true,
		opsPSec: rlOpsPSec,
		count:   rlItemCount,
		stopCh:  make(chan struct{}, 1),
		loop:    true,
	}

	go pumpBucketOps(opsType{count: rlItemCount}, rl)

	addAllNodesAtOnce("eventing")
	removeAllNodesAtOnce()

	rl.stopCh <- struct{}{}
}
