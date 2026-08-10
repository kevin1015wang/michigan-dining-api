package dynamoclient

import (
	"context"
	"math"
	"reflect"
	"time"

	pb "github.com/MichiganDiningAPI/proto/mdining"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
)

type DynamoClient struct {
	client       *dynamodb.Client
	streamClient *dynamodbstreams.Client
}

func New() *DynamoClient {
	dc := new(DynamoClient)
	// Using the SDK's default configuration, loading additional config
	// and credentials values from the environment variables, shared
	// credentials, and shared configuration files
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-1"))
	if err != nil {
		glog.Fatalf("Unable to load SDK config, %v", err)
	}
	dc.client = dynamodb.NewFromConfig(cfg)
	dc.streamClient = dynamodbstreams.NewFromConfig(cfg)
	return dc
}

func (d *DynamoClient) GetHearts(keys []string) (*[]*pb.HeartCount, error) {
	paramKeys := []map[string]types.AttributeValue{}
	for _, key := range keys {
		attributeValue, err := marshal(key)
		if err != nil {
			return nil, err
		}
		attributeKey := map[string]types.AttributeValue{
			HeartsTableKey: attributeValue,
		}
		paramKeys = append(paramKeys, attributeKey)
	}
	params := dynamodb.BatchGetItemInput{
		RequestItems: map[string]types.KeysAndAttributes{
			HeartsTableName: {Keys: paramKeys}}}

	resp, err := d.client.BatchGetItem(context.Background(), &params)
	if err != nil {
		return nil, err
	}
	heartCounts := []*pb.HeartCount{}
	for _, item := range resp.Responses[HeartsTableName] {
		heartCount := pb.HeartCount{}
		err = unmarshalMap(item, &heartCount)
		heartCounts = append(heartCounts, &heartCount)
	}
	for _, key := range resp.UnprocessedKeys[HeartsTableName].Keys {
		keyAttribute := key[HeartsTableKey]
		var keyValue string
		err := unmarshal(keyAttribute, &keyValue)
		if err != nil {
			continue
		}
		heartCount := pb.HeartCount{Key: keyValue, Count: 0}
		heartCounts = append(heartCounts, &heartCount)
	}
	return &heartCounts, nil
}

func (d *DynamoClient) AddHeart(key string) (*pb.HeartCount, error) {
	updateExpression := expression.Add(expression.Name("count"), expression.Value(1))
	expr, _ := expression.NewBuilder().WithUpdate(updateExpression).Build()
	dynamoKey, err := marshal(key)
	if err != nil {
		return nil, err
	}
	params := dynamodb.UpdateItemInput{
		TableName:                 &HeartsTableName,
		Key:                       map[string]types.AttributeValue{HeartsTableKey: dynamoKey},
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		UpdateExpression:          expr.Update(),
		ReturnValues:              types.ReturnValueAllNew,
	}
	resp, err := d.client.UpdateItem(context.Background(), &params)
	if err != nil {
		return nil, err
	}
	heartCount := pb.HeartCount{}
	err = unmarshalMap(resp.Attributes, &heartCount)
	if err != nil {
		return nil, err
	}
	return &heartCount, nil
}

func (d *DynamoClient) GetProto(table string, keys map[string]string, p proto.Message) error {
	dynamoKeys := make(map[string]types.AttributeValue)
	var keyErr error
	var k types.AttributeValue
	for keyName, key := range keys {
		k, keyErr = marshal(key)
		dynamoKeys[keyName] = k
	}
	if keyErr != nil {
		glog.Errorf("Error marshalling key to attribute: %s", keyErr)
		return keyErr
	}
	res, err := d.client.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: &table,
		Key:       dynamoKeys})
	if err != nil {
		glog.Errorf("Error sending get request for %s %s", reflect.TypeOf(p), err)
		return err
	}
	err = unmarshalMap(res.Item, p)
	if err != nil {
		glog.Errorf("Error unmarshalling response into %s %s", reflect.TypeOf(p), err)
		return err
	}
	glog.Infof("Succesfully Got %s", reflect.TypeOf(p))
	return nil
}

func (d *DynamoClient) PutProtoBatch(table *string, protos []proto.Message) error {
	reqs := make([]types.WriteRequest, 0)
	for _, p := range protos {
		av, err := marshalMap(p)
		if err != nil {
			return err
		}
		reqs = append(reqs, types.WriteRequest{PutRequest: &types.PutRequest{Item: av}})
	}
	numBatches := int(math.Ceil(float64(len(reqs)) / 25.0))
	currentBatch := 0
	problematicReqs := make([]types.WriteRequest, 0)
	for len(reqs) > 0 {
		// Take last 25 reqs (or all if <25 left)
		// Dynamo db restricts batch calls to 25 or fewer items
		startIdx := len(reqs) - 25
		if startIdx < 0 {
			startIdx = 0
		}
		out, err := d.client.BatchWriteItem(context.Background(), &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{
				*table: reqs[startIdx:]}})
		if err != nil {
			glog.Errorf("Error batch putting %s %s", reflect.TypeOf(protos), err)
			problematicReqs = append(problematicReqs, reqs[startIdx:]...)
		} else if unprocessed := out.UnprocessedItems[*table]; len(unprocessed) > 0 {
			// DynamoDB can throttle individual items within an otherwise
			// successful batch (e.g. under load right after switching to
			// on-demand capacity) without returning an error - those items
			// come back here instead of being written, so they must be
			// retried explicitly or they're silently dropped.
			glog.Warningf("%d unprocessed items batch putting %s, will retry individually", len(unprocessed), reflect.TypeOf(protos))
			problematicReqs = append(problematicReqs, unprocessed...)
		}
		reqs = reqs[:startIdx]
		glog.Infof("Batch Put %s (%d/%d): %d Items Remaining", *table, currentBatch, numBatches, len(reqs))
		currentBatch++
	}
	glog.Infof("Trying problematic requests individually (%d)", len(problematicReqs))
	for _, probReq := range problematicReqs {
		for attempt := 0; ; attempt++ {
			out, err := d.client.BatchWriteItem(context.Background(), &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{
					*table: {probReq}}})
			if err != nil {
				glog.Errorf("Error putting item %s", err)
				break
			}
			if len(out.UnprocessedItems[*table]) == 0 {
				break
			}
			if attempt >= 3 {
				glog.Errorf("Giving up on item in %s after %d retries, still unprocessed", *table, attempt+1)
				break
			}
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	glog.Infof("Successful Batch Put %s", reflect.TypeOf(protos))
	return nil
}

func (d *DynamoClient) PutProto(table *string, p proto.Message) error {
	// Convert from proto to dynamodb friendly structure
	av, err := marshalMap(p)
	if err != nil {
		return err
	}
	// Create and send put request
	_, err = d.client.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: table,
		Item:      av})
	if err != nil {
		glog.Errorf("Error putting item %s", err)
		return err
	}
	glog.Infof("Successfully Put %s", reflect.TypeOf(p))
	return nil
}
