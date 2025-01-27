package writebuffer

import (
	"math/rand"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/datanode/broker"
	"github.com/milvus-io/milvus/internal/datanode/metacache"
	"github.com/milvus-io/milvus/internal/datanode/syncmgr"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/tsoutil"
)

type BFWriteBufferSuite struct {
	suite.Suite
	collSchema *schemapb.CollectionSchema
	syncMgr    *syncmgr.MockSyncManager
	metacache  *metacache.MockMetaCache
	broker     *broker.MockBroker
}

func (s *BFWriteBufferSuite) SetupSuite() {
	paramtable.Get().Init(paramtable.NewBaseTable())
	s.collSchema = &schemapb.CollectionSchema{
		Name: "test_collection",
		Fields: []*schemapb.FieldSchema{
			{
				FieldID: common.RowIDField, Name: common.RowIDFieldName, DataType: schemapb.DataType_Int64,
			},
			{
				FieldID: common.TimeStampField, Name: common.TimeStampFieldName, DataType: schemapb.DataType_Int64,
			},
			{
				FieldID: 100, Name: "pk", DataType: schemapb.DataType_Int64, IsPrimaryKey: true,
			},
			{
				FieldID: 101, Name: "vector", DataType: schemapb.DataType_FloatVector,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: common.DimKey, Value: "128"},
				},
			},
		},
	}
}

func (s *BFWriteBufferSuite) composeInsertMsg(segmentID int64, rowCount int, dim int) ([]int64, *msgstream.InsertMsg) {
	tss := lo.RepeatBy(rowCount, func(idx int) int64 { return int64(tsoutil.ComposeTSByTime(time.Now(), int64(idx))) })
	vectors := lo.RepeatBy(rowCount, func(_ int) []float32 {
		return lo.RepeatBy(dim, func(_ int) float32 { return rand.Float32() })
	})
	flatten := lo.Flatten(vectors)
	return tss, &msgstream.InsertMsg{
		InsertRequest: msgpb.InsertRequest{
			SegmentID:  segmentID,
			Version:    msgpb.InsertDataVersion_ColumnBased,
			RowIDs:     tss,
			Timestamps: lo.Map(tss, func(id int64, _ int) uint64 { return uint64(id) }),
			FieldsData: []*schemapb.FieldData{
				{
					FieldId: common.RowIDField, FieldName: common.RowIDFieldName, Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: tss,
								},
							},
						},
					},
				},
				{
					FieldId: common.TimeStampField, FieldName: common.TimeStampFieldName, Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: tss,
								},
							},
						},
					},
				},
				{
					FieldId: common.StartOfUserFieldID, FieldName: "pk", Type: schemapb.DataType_Int64,
					Field: &schemapb.FieldData_Scalars{
						Scalars: &schemapb.ScalarField{
							Data: &schemapb.ScalarField_LongData{
								LongData: &schemapb.LongArray{
									Data: tss,
								},
							},
						},
					},
				},
				{
					FieldId: common.StartOfUserFieldID + 1, FieldName: "vector", Type: schemapb.DataType_FloatVector,
					Field: &schemapb.FieldData_Vectors{
						Vectors: &schemapb.VectorField{
							Dim: int64(dim),
							Data: &schemapb.VectorField_FloatVector{
								FloatVector: &schemapb.FloatArray{
									Data: flatten,
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *BFWriteBufferSuite) composeDeleteMsg(pks []storage.PrimaryKey) *msgstream.DeleteMsg {
	delMsg := &msgstream.DeleteMsg{
		DeleteRequest: msgpb.DeleteRequest{
			PrimaryKeys: storage.ParsePrimaryKeys2IDs(pks),
			Timestamps:  lo.RepeatBy(len(pks), func(idx int) uint64 { return tsoutil.ComposeTSByTime(time.Now(), int64(idx)) }),
		},
	}
	return delMsg
}

func (s *BFWriteBufferSuite) SetupTest() {
	s.syncMgr = syncmgr.NewMockSyncManager(s.T())
	s.metacache = metacache.NewMockMetaCache(s.T())
	s.broker = broker.NewMockBroker(s.T())
}

func (s *BFWriteBufferSuite) TestBufferData() {
	wb, err := NewBFWriteBuffer(s.collSchema, s.metacache, s.syncMgr, &writeBufferOption{})
	s.NoError(err)

	seg := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: 1000}, metacache.NewBloomFilterSet())
	s.metacache.EXPECT().GetSegmentsBy(mock.Anything).Return([]*metacache.SegmentInfo{seg})

	pks, msg := s.composeInsertMsg(1000, 10, 128)
	delMsg := s.composeDeleteMsg(lo.Map(pks, func(id int64, _ int) storage.PrimaryKey { return storage.NewInt64PrimaryKey(id) }))

	err = wb.BufferData([]*msgstream.InsertMsg{msg}, []*msgstream.DeleteMsg{delMsg}, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200})
	s.NoError(err)
}

func (s *BFWriteBufferSuite) TestAutoSync() {
	paramtable.Get().Save(paramtable.Get().DataNodeCfg.FlushInsertBufferSize.Key, "1")

	wb, err := NewBFWriteBuffer(s.collSchema, s.metacache, s.syncMgr, &writeBufferOption{
		syncPolicies: []SyncPolicy{
			SyncFullBuffer,
			GetSyncStaleBufferPolicy(paramtable.Get().DataNodeCfg.SyncPeriod.GetAsDuration(time.Second)),
			GetFlushingSegmentsPolicy(s.metacache),
		},
	})
	s.NoError(err)

	seg := metacache.NewSegmentInfo(&datapb.SegmentInfo{ID: 1000}, metacache.NewBloomFilterSet())
	s.metacache.EXPECT().GetSegmentsBy(mock.Anything).Return([]*metacache.SegmentInfo{seg})
	s.metacache.EXPECT().GetSegmentIDsBy(mock.Anything).Return([]int64{1002}) // mock flushing
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything, mock.Anything).Return()
	s.syncMgr.EXPECT().SyncData(mock.Anything, mock.Anything).Return(nil).Twice()

	pks, msg := s.composeInsertMsg(1000, 10, 128)
	delMsg := s.composeDeleteMsg(lo.Map(pks, func(id int64, _ int) storage.PrimaryKey { return storage.NewInt64PrimaryKey(id) }))

	err = wb.BufferData([]*msgstream.InsertMsg{msg}, []*msgstream.DeleteMsg{delMsg}, &msgpb.MsgPosition{Timestamp: 100}, &msgpb.MsgPosition{Timestamp: 200})
	s.NoError(err)
}

func TestBFWriteBuffer(t *testing.T) {
	suite.Run(t, new(BFWriteBufferSuite))
}
