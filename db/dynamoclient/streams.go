package dynamoclient

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/MichiganDiningAPI/proto/mdining"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	streamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"
	"github.com/golang/glog"
)

// convertStreamAttributeValue converts a dynamodbstreams AttributeValue (its own
// distinct union type) into the dynamodb AttributeValue type attributevalue.UnmarshalMap expects.
func convertStreamAttributeValue(v streamtypes.AttributeValue) types.AttributeValue {
	switch t := v.(type) {
	case *streamtypes.AttributeValueMemberB:
		return &types.AttributeValueMemberB{Value: t.Value}
	case *streamtypes.AttributeValueMemberBOOL:
		return &types.AttributeValueMemberBOOL{Value: t.Value}
	case *streamtypes.AttributeValueMemberBS:
		return &types.AttributeValueMemberBS{Value: t.Value}
	case *streamtypes.AttributeValueMemberL:
		converted := make([]types.AttributeValue, len(t.Value))
		for i, item := range t.Value {
			converted[i] = convertStreamAttributeValue(item)
		}
		return &types.AttributeValueMemberL{Value: converted}
	case *streamtypes.AttributeValueMemberM:
		return &types.AttributeValueMemberM{Value: convertStreamAttributeMap(t.Value)}
	case *streamtypes.AttributeValueMemberN:
		return &types.AttributeValueMemberN{Value: t.Value}
	case *streamtypes.AttributeValueMemberNS:
		return &types.AttributeValueMemberNS{Value: t.Value}
	case *streamtypes.AttributeValueMemberNULL:
		return &types.AttributeValueMemberNULL{Value: t.Value}
	case *streamtypes.AttributeValueMemberS:
		return &types.AttributeValueMemberS{Value: t.Value}
	case *streamtypes.AttributeValueMemberSS:
		return &types.AttributeValueMemberSS{Value: t.Value}
	}
	return nil
}

func convertStreamAttributeMap(m map[string]streamtypes.AttributeValue) map[string]types.AttributeValue {
	converted := make(map[string]types.AttributeValue, len(m))
	for k, v := range m {
		converted[k] = convertStreamAttributeValue(v)
	}
	return converted
}

func (d *DynamoClient) StreamHearts() (chan pb.HeartCount, chan struct{}) {
	heartCountChan := make(chan pb.HeartCount)
	recordChan, doneChan := d.streamRecords(HeartsTableName)
	go func(heartCountChan chan pb.HeartCount, recordChan chan streamtypes.Record) {
		for record := range recordChan {
			heartCount := pb.HeartCount{}
			err := unmarshalMap(convertStreamAttributeMap(record.Dynamodb.NewImage), &heartCount)
			if err != nil {
				glog.Warningf("Could not umarshal heart count: %s", err)
				continue
			}
			heartCountChan <- heartCount
		}
	}(heartCountChan, recordChan)
	return heartCountChan, doneChan
}

func (d *DynamoClient) streamRecords(table string) (chan streamtypes.Record, chan struct{}) {
	recordChan := make(chan streamtypes.Record)
	doneChan := make(chan struct{})
	go func(recordChan chan streamtypes.Record, doneChan chan struct{}, table string) {
		processingShards := map[string]struct{}{}
		ticker := time.NewTicker(5 * time.Minute)
		doneChans := []chan struct{}{}
		done := false
		mu := sync.Mutex{}
		shardPollCount := 0
		shardPollCountMu := sync.Mutex{}
		// Send done message to all shard specific doneChans if we get a done message
		go func(doneChan chan struct{}, recordChan chan streamtypes.Record) {
			<-doneChan
			mu.Lock()
			for _, done := range doneChans {
				done <- struct{}{}
			}
			close(recordChan)
			done = true
			mu.Unlock()
		}(doneChan, recordChan)
		for {
			mu.Lock()
			if done {
				break
			}
			streamArn, err := d.getTableStreamArn(table)
			if err != nil {
				glog.Errorf("Failed to get stream arn, will retry: %s", err)
				mu.Unlock()
				time.Sleep(time.Second)
				continue
			}
			glog.Infof("Stream Arn: %s", *streamArn)
			shards, err := d.getStreamShards(*streamArn)
			if err != nil {
				glog.Errorf("Failed to get stream shards, will retry: %s", err)
				mu.Unlock()
				time.Sleep(time.Second)
				continue
			}
			recordChans := []chan streamtypes.Record{}
			// For each shard, start polling for records
			for _, shard := range *shards {
				_, alreadyProcessing := processingShards[*shard.ShardId]
				if alreadyProcessing {
					continue
				}
				glog.Infof("Processing new shard: %s", *shard.ShardId)
				processingShards[*shard.ShardId] = struct{}{}
				shardIt, err := d.getShardIterator(*streamArn, shard)
				if err != nil {
					glog.Errorf("Failed to get shard iterator for shard %s, skipping: %s", *shard.ShardId, err)
					delete(processingShards, *shard.ShardId)
					continue
				}
				shardPollCountMu.Lock()
				shardPollCount++
				shardPollCountMu.Unlock()
				records, done := d.pollShardForRecords(*shardIt)
				recordChans = append(recordChans, records)
				doneChans = append(doneChans, done)
			}
			// Aggregate each shard specific recordChan into one channel
			for _, records := range recordChans {
				go func(records chan streamtypes.Record) {
					for record := range records {
						recordChan <- record
					}
					shardPollCountMu.Lock()
					shardPollCount--
					shardPollCountMu.Unlock()
					glog.Infof("Closed records")
				}(records)
			}
			shardPollCountMu.Lock()
			glog.Infof("%d shard pollers active for table %s", shardPollCount, table)
			shardPollCountMu.Unlock()
			mu.Unlock()
			// Wait for next tick to check for new shards
			<-ticker.C
		}
	}(recordChan, doneChan, table)
	return recordChan, doneChan
}

func (d *DynamoClient) pollShardForRecords(shardIterator string) (chan streamtypes.Record, chan struct{}) {
	recordChan := make(chan streamtypes.Record)
	doneChan := make(chan struct{})
	go func(recordChan chan streamtypes.Record, doneChan chan struct{}, shardIterator string) {
		shardIt := &shardIterator
		defer close(recordChan)
		for shardIt != nil {
			select {
			case <-doneChan:
				return
			default:
				var records *[]streamtypes.Record
				var err error
				shardIt, records, err = d.getRecords(*shardIt)
				if err != nil {
					glog.Warningf("Failed to get records")
					close(recordChan)
				}
				for _, record := range *records {
					recordChan <- record
				}
				if len(*records) == 0 {
					time.Sleep(time.Second)
				}
			}
		}
	}(recordChan, doneChan, shardIterator)
	return recordChan, doneChan
}

func (d *DynamoClient) getRecords(shardIterator string) (*string, *[]streamtypes.Record, error) {
	params := dynamodbstreams.GetRecordsInput{
		ShardIterator: &shardIterator,
	}
	resp, err := d.streamClient.GetRecords(context.Background(), &params)
	if err != nil {
		return nil, nil, err
	}
	return resp.NextShardIterator, &resp.Records, nil
}

func (d *DynamoClient) getShardIterator(arn string, shard streamtypes.Shard) (*string, error) {
	params := dynamodbstreams.GetShardIteratorInput{
		StreamArn:         &arn,
		ShardId:           shard.ShardId,
		ShardIteratorType: streamtypes.ShardIteratorTypeLatest,
	}
	resp, err := d.streamClient.GetShardIterator(context.Background(), &params)
	if err != nil {
		return nil, err
	}
	return resp.ShardIterator, nil
}

func (d *DynamoClient) getStreamShards(arn string) (*[]streamtypes.Shard, error) {
	params := dynamodbstreams.DescribeStreamInput{
		StreamArn: &arn,
	}
	resp, err := d.streamClient.DescribeStream(context.Background(), &params)
	if err != nil {
		return nil, err
	}
	return &resp.StreamDescription.Shards, nil
}

func (d *DynamoClient) getTableStreamArn(table string) (*string, error) {
	// Example sending a request using the ListStreams method.
	params := dynamodbstreams.ListStreamsInput{
		TableName: &table,
	}
	resp, err := d.streamClient.ListStreams(context.Background(), &params)
	if err != nil { // resp is now filled
		return nil, err
	}
	if len(resp.Streams) == 0 {
		return nil, errors.New("No streams found")
	}
	return resp.Streams[0].StreamArn, nil
}
