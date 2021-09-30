package source

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/squareup/pranadb/perrors"
	"reflect"
	"strings"

	"github.com/PaesslerAG/gval"
	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/kafka"
	"github.com/squareup/pranadb/protolib"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

var (
	jsonDecoder         = &JSONDecoder{}
	kafkaDecoderFloat   = newKafkaDecoder(common.KafkaEncodingFloat32BE)
	kafkaDecoderDouble  = newKafkaDecoder(common.KafkaEncodingFloat64BE)
	kafkaDecoderInteger = newKafkaDecoder(common.KafkaEncodingInt32BE)
	kafkaDecoderLong    = newKafkaDecoder(common.KafkaEncodingInt64BE)
	kafkaDecoderShort   = newKafkaDecoder(common.KafkaEncodingInt16BE)
	kafkaDecoderString  = newKafkaDecoder(common.KafkaEncodingStringBytes)

	evalCtx = context.Background()
)

type MessageParser struct {
	sourceInfo       *common.SourceInfo
	rowsFactory      *common.RowsFactory
	colEvals         []evaluable
	headerDecoder    Decoder
	keyDecoder       Decoder
	valueDecoder     Decoder
	repMap           map[string]interface{}
	protobufRegistry protolib.Resolver
}

func NewMessageParser(sourceInfo *common.SourceInfo, registry protolib.Resolver) (*MessageParser, error) {
	selectors := sourceInfo.TopicInfo.ColSelectors
	selectEvals := make([]evaluable, len(selectors))
	// We pre-compute whether the selectors need headers, key and value so we don't unnecessary parse them if they
	// don't use them
	var (
		headerDecoder, keyDecoder, valueDecoder Decoder

		encoding common.KafkaEncoding
		err      error
		eval     evaluable
	)
	topic := sourceInfo.TopicInfo
	for i, selector := range selectors {
		switch {
		case strings.HasPrefix(selector, "h"):
			encoding = topic.HeaderEncoding
			headerDecoder, err = getDecoder(registry, encoding)
		case strings.HasPrefix(selector, "k"):
			encoding = topic.KeyEncoding
			keyDecoder, err = getDecoder(registry, encoding)
		case strings.HasPrefix(selector, "v"):
			encoding = topic.ValueEncoding
			valueDecoder, err = getDecoder(registry, encoding)
		case strings.HasPrefix(selector, "t"):
			// timestamp selector, no decoding required
		default:
			panic(fmt.Sprintf("invalid selector %q", selector))
		}
		if err != nil {
			return nil, err
		}

		if encoding.Encoding == common.EncodingProtobuf {
			eval, err = protoEvaluable(selector)
		} else {
			eval, err = gvalEvaluable(selector)
		}
		if err != nil {
			return nil, err
		}
		selectEvals[i] = eval
	}
	repMap := make(map[string]interface{}, 4)
	return &MessageParser{
		rowsFactory:      common.NewRowsFactory(sourceInfo.ColumnTypes),
		protobufRegistry: registry,
		sourceInfo:       sourceInfo,
		colEvals:         selectEvals,
		headerDecoder:    headerDecoder,
		keyDecoder:       keyDecoder,
		valueDecoder:     valueDecoder,
		repMap:           repMap,
	}, nil
}

func (m *MessageParser) ParseMessages(messages []*kafka.Message) (*common.Rows, error) {
	rows := m.rowsFactory.NewRows(len(messages))
	for _, msg := range messages {
		if err := m.decodeMessage(msg); err != nil {
			return nil, errors.WithStack(err)
		}
		if err := m.evalColumns(rows); err != nil {
			return nil, errors.WithStack(err)
		}
	}
	return rows, nil
}

func (m *MessageParser) decodeMessage(message *kafka.Message) error {
	// Decode headers
	var hdrs map[string]interface{}
	if m.headerDecoder != nil {
		lh := len(message.Headers)
		if lh > 0 {
			hdrs = make(map[string]interface{}, lh)
			for _, hdr := range message.Headers {
				hm, err := m.decodeBytes(m.headerDecoder, hdr.Value)
				if err != nil {
					return err
				}
				hdrs[hdr.Key] = hm
			}
		}
	}
	// Decode key
	var km interface{}
	if m.keyDecoder != nil {
		var err error
		km, err = m.decodeBytes(m.keyDecoder, message.Key)
		if err != nil {
			return err
		}
	}
	// Decode value
	var vm interface{}
	if m.valueDecoder != nil {
		var err error
		vm, err = m.decodeBytes(m.valueDecoder, message.Value)
		if err != nil {
			return err
		}
	}

	m.repMap["h"] = hdrs
	m.repMap["k"] = km
	m.repMap["v"] = vm
	m.repMap["t"] = message.TimeStamp

	return nil
}

func (m *MessageParser) evalColumns(rows *common.Rows) error {
	for i, eval := range m.colEvals {
		colType := m.sourceInfo.ColumnTypes[i]
		val, err := eval(m.repMap)
		if err != nil {
			return err
		}
		if val == nil {
			rows.AppendNullToColumn(i)
			continue
		}
		switch colType.Type {
		case common.TypeTinyInt, common.TypeInt, common.TypeBigInt:
			ival, err := CoerceInt64(val)
			if err != nil {
				return err
			}
			rows.AppendInt64ToColumn(i, ival)
		case common.TypeDouble:
			fval, err := CoerceFloat64(val)
			if err != nil {
				return err
			}
			rows.AppendFloat64ToColumn(i, fval)
		case common.TypeVarchar:
			sval, err := CoerceString(val)
			if err != nil {
				return err
			}
			rows.AppendStringToColumn(i, sval)
		case common.TypeDecimal:
			dval, err := CoerceDecimal(val)
			if err != nil {
				return err
			}
			rows.AppendDecimalToColumn(i, *dval)
		case common.TypeTimestamp:
			tsVal, err := CoerceTimestamp(val)
			if err != nil {
				return err
			}
			rows.AppendTimestampToColumn(i, tsVal)
		default:
			return perrors.Errorf("unsupported col type %d", colType.Type)
		}
	}
	return nil
}

func getDecoder(registry protolib.Resolver, encoding common.KafkaEncoding) (Decoder, error) {
	var decoder Decoder
	switch encoding.Encoding {
	case common.EncodingJSON:
		decoder = jsonDecoder
	case common.EncodingFloat64BE:
		decoder = kafkaDecoderDouble
	case common.EncodingFloat32BE:
		decoder = kafkaDecoderFloat
	case common.EncodingInt32BE:
		decoder = kafkaDecoderInteger
	case common.EncodingInt64BE:
		decoder = kafkaDecoderLong
	case common.EncodingInt16BE:
		decoder = kafkaDecoderShort
	case common.EncodingStringBytes:
		decoder = kafkaDecoderString
	case common.EncodingProtobuf:
		desc, err := registry.FindDescriptorByName(protoreflect.FullName(encoding.SchemaName))
		if err != nil {
			return nil, perrors.Errorf("could not find protobuf descriptor for %q", encoding.SchemaName)
		}
		msgDesc, ok := desc.(protoreflect.MessageDescriptor)
		if !ok {
			return nil, perrors.Errorf("expected to find MessageDescriptor at %q, but was %q", encoding.SchemaName, reflect.TypeOf(msgDesc))
		}
		decoder = &ProtobufDecoder{desc: msgDesc}
	default:
		panic("unsupported encoding")
	}
	return decoder, nil
}

func (m *MessageParser) decodeBytes(decoder Decoder, bytes []byte) (interface{}, error) {
	if bytes == nil {
		return nil, nil
	}
	v, err := decoder.Decode(bytes)
	return v, errors.WithStack(err)
}

type Decoder interface {
	Decode(bytes []byte) (interface{}, error)
}

type JSONDecoder struct {
}

func (j *JSONDecoder) Decode(bytes []byte) (interface{}, error) {
	m := make(map[string]interface{})
	if err := json.Unmarshal(bytes, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func newKafkaDecoder(encoding common.KafkaEncoding) *KafkaDecoder {
	return &KafkaDecoder{encoding: encoding}
}

type KafkaDecoder struct {
	encoding common.KafkaEncoding
}

func (k *KafkaDecoder) Decode(bytes []byte) (interface{}, error) {
	if len(bytes) == 0 {
		return nil, nil
	}
	switch k.encoding.Encoding {
	case common.EncodingFloat32BE:
		val, _ := common.ReadFloat32FromBufferBE(bytes, 0)
		return val, nil
	case common.EncodingFloat64BE:
		val, _ := common.ReadFloat64FromBufferBE(bytes, 0)
		return val, nil
	case common.EncodingInt32BE:
		val, _ := common.ReadUint32FromBufferBE(bytes, 0)
		return int32(val), nil
	case common.EncodingInt64BE:
		val, _ := common.ReadUint64FromBufferBE(bytes, 0)
		return int64(val), nil
	case common.EncodingInt16BE:
		val, _ := common.ReadUint16FromBufferBE(bytes, 0)
		return int16(val), nil
	case common.EncodingStringBytes:
		// UTF-8 encoded
		return string(bytes), nil
	default:
		panic("unknown encoding")
	}
}

type ProtobufDecoder struct {
	desc protoreflect.MessageDescriptor
}

func (p *ProtobufDecoder) Decode(bytes []byte) (interface{}, error) {
	msg := dynamicpb.NewMessage(p.desc)
	err := proto.Unmarshal(bytes, msg)
	return msg, err
}

type evaluable func(repMap map[string]interface{}) (interface{}, error)

func gvalEvaluable(selector string) (evaluable, error) {
	e, err := gval.Base().NewEvaluable(selector)
	if err != nil {
		return nil, err
	}
	return func(repMap map[string]interface{}) (interface{}, error) {
		return e(evalCtx, repMap)
	}, nil
}

func protoEvaluable(selector string) (evaluable, error) {
	prefix := selector[0:1]
	if len(selector) <= 2 || selector[1] != '.' {
		return nil, perrors.Errorf("invalid protobuf selector %q", selector)
	}
	sel, err := protolib.ParseSelector(selector[2:])
	if err != nil {
		return nil, err
	}
	return func(repMap map[string]interface{}) (interface{}, error) {
		return sel.Select(repMap[prefix].(protoreflect.Message))
	}, nil
}
