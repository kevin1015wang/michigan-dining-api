package dynamoclient

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/golang/glog"
)

func (d *DynamoClient) DeleteTables() error {
	glog.Info("Deleting all tables...")
	for _, table := range TableNames {
		d.deleteTable(table)
	}
	return nil
}

func (d *DynamoClient) deleteTable(table string) {
	_, err := d.client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{
		TableName: &table})
	if err != nil {
		glog.Fatalf("Failed to delete table %s %v", table, err)
	}
	glog.Infof("Deleted table %s.", table)
}
