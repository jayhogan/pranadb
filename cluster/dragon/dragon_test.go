package dragon

import (
	"errors"
	"fmt"
	"github.com/squareup/pranadb/cluster"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"testing"
	"time"
)

var dragonCluster []cluster.Cluster

const (
	numShards = 10
)

func TestMain(m *testing.M) {
	dataDir, err := ioutil.TempDir("", "dragon-test")
	if err != nil {
		panic("failed to create temp dir")
	}
	dragonCluster, err = startDragonCluster(dataDir)
	if err != nil {
		panic(fmt.Sprintf("failed to start dragon cluster %v", err))
	}
	defer func() {
		stopDragonCluster()
		defer func() {
			// We want to do this even if stopDragonCluster fails hence the extra defer
			err := os.RemoveAll(dataDir)
			if err != nil {
				panic("failed to remove test dir")
			}
		}()
	}()
	m.Run()
}

func getLocalNodeAndLocalShard() (cluster.Cluster, uint64) {
	clust := dragonCluster[0]
	shardID := clust.GetLocalShardIDs()[0]
	return clust, shardID
}

func TestLocalPutGet(t *testing.T) {
	node, localShard := getLocalNodeAndLocalShard()

	key := []byte("somekey")
	value := []byte("somevalue")

	kvPair := cluster.KVPair{
		Key:   key,
		Value: value,
	}

	writeBatch := createWriteBatchWithPuts(localShard, kvPair)

	err := node.WriteBatch(&writeBatch)
	require.Nil(t, err)

	res, err := node.LocalGet(key)
	require.Nil(t, err)
	require.NotNil(t, res)

	require.Equal(t, string(value), string(res))
}

func TestLocalPutDelete(t *testing.T) {
	node, localShard := getLocalNodeAndLocalShard()

	key := []byte("somekey")
	value := []byte("somevalue")

	kvPair := cluster.KVPair{
		Key:   key,
		Value: value,
	}

	writeBatch := createWriteBatchWithPuts(localShard, kvPair)

	err := node.WriteBatch(&writeBatch)
	require.Nil(t, err)

	res, err := node.LocalGet(key)
	require.Nil(t, err)
	require.NotNil(t, res)

	deleteBatch := createWriteBatchWithDeletes(localShard, key)

	err = node.WriteBatch(&deleteBatch)
	require.Nil(t, err)

	res, err = node.LocalGet(key)
	require.Nil(t, err)
	require.Nil(t, res)
}

func TestLocalScan(t *testing.T) {
	testLocalScan(t, -1, 10)
}

func TestLocalScanSmallLimit(t *testing.T) {
	testLocalScan(t, 3, 3)
}

func TestLocalScanBigLimit(t *testing.T) {
	testLocalScan(t, 1000, 10)
}

func testLocalScan(t *testing.T, limit int, expected int) {
	t.Helper()
	node, localShard := getLocalNodeAndLocalShard()

	var kvPairs []cluster.KVPair
	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			k := []byte(fmt.Sprintf("foo-%02d/bar-%02d", i, j))
			v := []byte(fmt.Sprintf("somevalue%02d", j))
			kvPairs = append(kvPairs, cluster.KVPair{Key: k, Value: v})
		}
	}
	rand.Shuffle(len(kvPairs), func(i, j int) {
		kvPairs[i], kvPairs[j] = kvPairs[j], kvPairs[i]
	})

	wb := cluster.NewWriteBatch(localShard, false)
	for _, kvPair := range kvPairs {
		wb.AddPut(kvPair.Key, kvPair.Value)
	}

	err := node.WriteBatch(wb)
	require.Nil(t, err)

	keyStart := []byte("foo-06")
	keyWhile := []byte("foo-06")

	var res []cluster.KVPair
	res, err = node.LocalScan(keyStart, keyWhile, limit)
	require.Nil(t, err)

	require.Equal(t, expected, len(res))
	for i, kvPair := range res {
		log.Printf("key is: %s", string(kvPair.Key))
		expectedK := fmt.Sprintf("foo-06/bar-%02d", i)
		expectedV := fmt.Sprintf("somevalue%02d", i)
		require.Equal(t, expectedK, string(kvPair.Key))
		require.Equal(t, expectedV, string(kvPair.Value))
	}
}

func TestGenerateTableID(t *testing.T) {
	for i := 0; i < 10; i++ {
		id, err := dragonCluster[i%len(dragonCluster)].GenerateTableID()
		require.Nil(t, err)
		require.Equal(t, uint64(i), id)
	}
}

func TestGetNodeID(t *testing.T) {
	for i := 0; i < len(dragonCluster); i++ {
		require.Equal(t, i, dragonCluster[i].GetNodeID())
	}
}

func TestGetAllNodeIDs(t *testing.T) {
	for i := 0; i < len(dragonCluster); i++ {
		allNodeIds := dragonCluster[i].GetAllNodeIDs()
		require.Equal(t, len(dragonCluster), len(allNodeIds))
		nids := map[int]struct{}{}
		for _, nid := range allNodeIds {
			nids[nid] = struct{}{}
		}
		require.Equal(t, len(dragonCluster), len(nids))
		for i := 0; i < len(dragonCluster); i++ {
			_, ok := nids[i]
			require.True(t, ok)
		}
	}
}

func TestGetAllShardIDs(t *testing.T) {
	for i := 0; i < len(dragonCluster); i++ {
		allShardIds := dragonCluster[i].GetAllShardIDs()
		require.Equal(t, numShards, len(allShardIds))
	}
}

func stopDragonCluster() {
	for _, dragon := range dragonCluster {
		err := dragon.Stop()
		if err != nil {
			panic(fmt.Sprintf("failed to stop dragon cluster %v", err))
		}
	}
}

func startDragonCluster(dataDir string) ([]cluster.Cluster, error) {

	nodeAddresses := []string{
		"localhost:63001",
		"localhost:63002",
		"localhost:63003",
	}

	chans := make([]chan error, len(nodeAddresses))
	clusterNodes := make([]cluster.Cluster, len(nodeAddresses))
	for i := 0; i < len(chans); i++ {
		ch := make(chan error, 1)
		chans[i] = ch
		dragon, err := NewDragon(i, nodeAddresses, numShards, dataDir, 3)
		if err != nil {
			return nil, err
		}
		clusterNodes[i] = dragon
		dragon.RegisterShardListenerFactory(&dummyShardListenerFactory{})

		go startDragonNode(dragon, ch)
	}

	for i := 0; i < len(chans); i++ {
		err, ok := <-chans[i]
		if !ok {
			return nil, errors.New("channel was closed")
		}
		if err != nil {
			return nil, err
		}
	}

	time.Sleep(5 * time.Second)
	return clusterNodes, nil
}

func startDragonNode(dragon cluster.Cluster, ch chan error) {
	err := dragon.Start()
	ch <- err
}

func createWriteBatchWithPuts(shardID uint64, puts ...cluster.KVPair) cluster.WriteBatch {
	wb := cluster.NewWriteBatch(shardID, false)
	for _, kvPair := range puts {
		wb.AddPut(kvPair.Key, kvPair.Value)
	}
	return *wb
}

func createWriteBatchWithDeletes(shardID uint64, deletes ...[]byte) cluster.WriteBatch {
	wb := cluster.NewWriteBatch(shardID, false)
	for _, delete := range deletes {
		wb.AddDelete(delete)
	}
	return *wb
}

type dummyShardListenerFactory struct {
}

func (d *dummyShardListenerFactory) CreateShardListener(shardID uint64) cluster.ShardListener {
	return &dummyShardListener{}
}

type dummyShardListener struct {
}

func (d *dummyShardListener) RemoteWriteOccurred() {
}

func (d *dummyShardListener) Close() {
}