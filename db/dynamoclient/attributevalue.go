package dynamoclient

import (
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The protobuf-generated message structs only carry `json` struct tags, not
// `dynamodbav` ones. attributevalue's default encoder/decoder only reads
// `dynamodbav`, falling back to the raw Go field name (e.g. "DiningHallMeal")
// rather than the table's key schema attribute name (e.g. "diningHallMeal"),
// which breaks key matching entirely. The old, pre-GA AWS SDK this project
// originally used fell back to `json` tags automatically; the current SDK
// requires it to be configured explicitly.
var avEncoder = attributevalue.NewEncoder(func(o *attributevalue.EncoderOptions) { o.TagKey = "json" })
var avDecoder = attributevalue.NewDecoder(func(o *attributevalue.DecoderOptions) { o.TagKey = "json" })

func marshal(in interface{}) (types.AttributeValue, error) {
	return avEncoder.Encode(in)
}

func marshalMap(in interface{}) (map[string]types.AttributeValue, error) {
	av, err := avEncoder.Encode(in)
	if err != nil {
		return nil, err
	}
	m, ok := av.(*types.AttributeValueMemberM)
	if !ok {
		return map[string]types.AttributeValue{}, nil
	}
	return m.Value, nil
}

func unmarshal(av types.AttributeValue, out interface{}) error {
	return avDecoder.Decode(av, out)
}

func unmarshalMap(m map[string]types.AttributeValue, out interface{}) error {
	return avDecoder.Decode(&types.AttributeValueMemberM{Value: m}, out)
}
