package table

import (
	"github.com/squareup/pranadb/cluster"
	"github.com/squareup/pranadb/common"
	"log"
)

func Upsert(tableInfo *common.TableInfo, row *common.Row, writeBatch *cluster.WriteBatch) error {
	keyBuff, err := encodeKeyFromRow(tableInfo, row, writeBatch.ShardID)
	if err != nil {
		return err
	}
	var valueBuff []byte
	valueBuff, err = common.EncodeRow(row, tableInfo.ColumnTypes, valueBuff)
	if err != nil {
		return err
	}
	writeBatch.AddPut(keyBuff, valueBuff)
	return nil
}

func Delete(tableInfo *common.TableInfo, row *common.Row, writeBatch *cluster.WriteBatch) error {
	keyBuff, err := encodeKeyFromRow(tableInfo, row, writeBatch.ShardID)
	if err != nil {
		return err
	}
	writeBatch.AddDelete(keyBuff)
	return nil
}

func LookupInPk(tableInfo *common.TableInfo, key common.Key, keyColIndexes []int, shardID uint64, rowsFactory *common.RowsFactory, storage cluster.Cluster) (*common.Row, error) {
	buffer, err := encodeKey(tableInfo, key, keyColIndexes, shardID)
	if err != nil {
		return nil, err
	}

	log.Printf("Looking up key %v in table %d", buffer, tableInfo.ID)
	buffRes, err := storage.LocalGet(buffer)
	if err != nil {
		return nil, err
	}
	if buffRes == nil {
		return nil, nil
	}
	log.Printf("Got k:%v v:%v", buffer, buffRes)

	rows := rowsFactory.NewRows(1)
	err = common.DecodeRow(buffRes, tableInfo.ColumnTypes, rows)
	if err != nil {
		return nil, err
	}
	if rows.RowCount() != 1 {
		panic("expected one row")
	}
	row := rows.GetRow(0)
	return &row, nil
}

func EncodeTableKeyPrefix(tableID uint64, shardID uint64, capac int) []byte {
	keyBuff := make([]byte, 0, capac)
	// Data key must be in big-endian order so that byte-by-byte key comparison correctly orders the keys
	keyBuff = common.AppendUint64ToBufferBigEndian(keyBuff, shardID)
	keyBuff = common.AppendUint64ToBufferBigEndian(keyBuff, tableID)
	return keyBuff
}

// TODO key cols must be in bigendian too!
func encodeKeyFromRow(tableInfo *common.TableInfo, row *common.Row, shardID uint64) ([]byte, error) {
	keyBuff := EncodeTableKeyPrefix(tableInfo.ID, shardID, 32)
	return common.EncodeCols(row, tableInfo.PrimaryKeyCols, tableInfo.ColumnTypes, keyBuff, false)
}

// TODO key cols must be in bigendian too!
func encodeKey(tableInfo *common.TableInfo, key common.Key, keyColIndexes []int, shardID uint64) ([]byte, error) {
	keyBuff := EncodeTableKeyPrefix(tableInfo.ID, shardID, 32)
	return common.EncodeKey(key, tableInfo.ColumnTypes, keyColIndexes, keyBuff)
}
