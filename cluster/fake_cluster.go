package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/google/btree"

	"github.com/squareup/pranadb/common"
)

type FakeCluster struct {
	nodeID                       int
	mu                           sync.RWMutex
	tableSequence                uint64
	remoteQueryExecutionCallback RemoteQueryExecutionCallback
	allShardIds                  []uint64
	started                      bool
	btree                        *btree.BTree
	shardListenerFactory         ShardListenerFactory
	shardListeners               map[uint64]ShardListener
	notifListeners               map[NotificationType]NotificationListener
	membershipListener           MembershipListener
}

func NewFakeCluster(nodeID int, numShards int) *FakeCluster {
	return &FakeCluster{
		nodeID:         nodeID,
		tableSequence:  uint64(common.UserTableIDBase), // First 100 reserved for system tables
		allShardIds:    genAllShardIds(numShards),
		btree:          btree.New(3),
		shardListeners: make(map[uint64]ShardListener),
		notifListeners: make(map[NotificationType]NotificationListener),
	}
}

func (f *FakeCluster) BroadcastNotification(notification Notification) error {
	listener := f.lookupNotificationListener(notification)
	listener.HandleNotification(notification)
	return nil
}

func (f *FakeCluster) lookupNotificationListener(notification Notification) NotificationListener {
	f.mu.RLock()
	defer f.mu.RUnlock()
	listener, ok := f.notifListeners[TypeForNotification(notification)]
	if !ok {
		panic(fmt.Sprintf("no notification listener for type %d", TypeForNotification(notification)))
	}
	return listener
}

func (f *FakeCluster) RegisterNotificationListener(notificationType NotificationType, listener NotificationListener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.notifListeners[notificationType]
	if ok {
		panic(fmt.Sprintf("notification listener with type %d already registered", notificationType))
	}
	f.notifListeners[notificationType] = listener
}

func (f *FakeCluster) RegisterMembershipListener(listener MembershipListener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.membershipListener != nil {
		panic("membership listener already registered")
	}
	f.membershipListener = listener
}

func (f *FakeCluster) ExecuteRemotePullQuery(queryInfo *QueryExecutionInfo, rowsFactory *common.RowsFactory) (*common.Rows, error) {
	return f.remoteQueryExecutionCallback.ExecuteRemotePullQuery(queryInfo)
}

func (f *FakeCluster) SetRemoteQueryExecutionCallback(callback RemoteQueryExecutionCallback) {
	f.remoteQueryExecutionCallback = callback
}

func (f *FakeCluster) RegisterShardListenerFactory(factory ShardListenerFactory) {
	f.shardListenerFactory = factory
}

func (f *FakeCluster) GetNodeID() int {
	return f.nodeID
}

func (f *FakeCluster) GetAllShardIDs() []uint64 {
	return f.allShardIds
}

func (f *FakeCluster) GetLocalShardIDs() []uint64 {
	return f.allShardIds
}

func (f *FakeCluster) GenerateTableID() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.tableSequence
	f.tableSequence++
	return res, nil
}

func (f *FakeCluster) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remoteQueryExecutionCallback == nil {
		panic("remote query execution callback must be set before start")
	}
	if f.shardListenerFactory == nil {
		panic("shard listener factory must be set before start")
	}
	if f.started {
		return nil
	}
	f.startShardListeners()
	f.started = true
	return nil
}

// Stop resets all ephemeral state for a cluster, allowing it to be used with a new
// server but keeping all persisted data.
func (f *FakeCluster) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return nil
	}
	f.remoteQueryExecutionCallback = nil
	f.shardListenerFactory = nil
	f.shardListeners = make(map[uint64]ShardListener)
	f.notifListeners = make(map[NotificationType]NotificationListener)
	f.membershipListener = nil
	f.started = false
	return nil
}

func (f *FakeCluster) WriteBatch(batch *WriteBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if batch.ShardID < DataShardIDBase {
		panic(fmt.Sprintf("invalid shard cluster id %d", batch.ShardID))
	}
	log.Printf("Write batch for shard %d", batch.ShardID)
	log.Printf("Writing batch, puts %d, Deletes %d", len(batch.puts.TheMap), len(batch.Deletes.TheMap))
	for k, v := range batch.puts.TheMap {
		kBytes := common.StringToByteSliceZeroCopy(k)
		log.Printf("Putting key %v value %v", kBytes, v)
		f.putInternal(&kvWrapper{
			key:   kBytes,
			value: v,
		})
	}
	for k := range batch.Deletes.TheMap {
		kBytes := common.StringToByteSliceZeroCopy(k)
		log.Printf("Deleting key %v", kBytes)
		err := f.deleteInternal(&kvWrapper{
			key: kBytes,
		})
		if err != nil {
			return err
		}
	}
	if batch.NotifyRemote {
		shardListener := f.shardListeners[batch.ShardID]
		go shardListener.RemoteWriteOccurred()
	}
	return nil
}

func (f *FakeCluster) LocalGet(key []byte) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.getInternal(&kvWrapper{key: key}), nil
}

func (f *FakeCluster) DeleteAllDataInRange(startPrefix []byte, endPrefix []byte) error {
	log.Printf("Deleting data in range %v to %v", startPrefix, endPrefix)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, shardID := range f.allShardIds {
		startPref := make([]byte, 0, 16)
		startPref = common.AppendUint64ToBufferBigEndian(startPref, shardID)
		startPref = append(startPref, startPrefix...)

		endPref := make([]byte, 0, 16)
		endPref = common.AppendUint64ToBufferBigEndian(endPref, shardID)
		endPref = append(endPref, endPrefix...)

		pairs, err := f.LocalScan(startPref, endPref, -1)
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			err := f.deleteInternal(&kvWrapper{
				key: pair.Key,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *FakeCluster) LocalScan(startKeyPrefix []byte, endKeyPrefix []byte, limit int) ([]KVPair, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if startKeyPrefix == nil {
		panic("startKeyPrefix cannot be nil")
	}
	var result []KVPair
	count := 0
	resFunc := func(i btree.Item) bool {
		wrapper := i.(*kvWrapper) // nolint: forcetypeassert
		if endKeyPrefix != nil && bytes.Compare(wrapper.key, endKeyPrefix) >= 0 {
			return false
		}
		result = append(result, KVPair{
			Key:   wrapper.key,
			Value: wrapper.value,
		})
		count++
		return limit == -1 || count < limit
	}
	f.btree.AscendGreaterOrEqual(&kvWrapper{key: startKeyPrefix}, resFunc)
	return result, nil
}

func (f *FakeCluster) startShardListeners() {
	if f.shardListenerFactory == nil {
		return
	}
	for _, shardID := range f.allShardIds {
		shardListener := f.shardListenerFactory.CreateShardListener(shardID)
		f.shardListeners[shardID] = shardListener
	}
}

func genAllShardIds(numShards int) []uint64 {
	allShards := make([]uint64, numShards)
	for i := 0; i < numShards; i++ {
		allShards[i] = uint64(i) + DataShardIDBase
	}
	return allShards
}

type kvWrapper struct {
	key   []byte
	value []byte
}

func (k kvWrapper) Less(than btree.Item) bool {
	otherKVwrapper := than.(*kvWrapper) // nolint: forcetypeassert

	thisKey := k.key
	otherKey := otherKVwrapper.key

	return bytes.Compare(thisKey, otherKey) < 0
}

func (f *FakeCluster) putInternal(item *kvWrapper) {
	f.btree.ReplaceOrInsert(item)
}

func (f *FakeCluster) deleteInternal(item *kvWrapper) error {
	prevItem := f.btree.Delete(item)
	if prevItem == nil {
		return errors.New("didn't find item to delete")
	}
	return nil
}

func (f *FakeCluster) getInternal(key *kvWrapper) []byte {
	if item := f.btree.Get(key); item != nil {
		wrapper := item.(*kvWrapper) // nolint: forcetypeassert
		return wrapper.value
	}
	return nil
}
