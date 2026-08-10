package dynamoclient

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/golang/glog"
)

func (d *DynamoClient) CreateTablesIfNotExists() {
	glog.Info("Checking for existence of dynamodb tables...")
	for _, table := range TableNames {
		if !d.tableExists(table) {
			glog.Infof("Table %s does not exist. Creating now...", table)
			d.createTable(table)
		} else {
			glog.Infof("Table %s exists.", table)
		}
	}
}

func (d *DynamoClient) CreateTables() error {
	glog.Info("Creating dynamodb tables...")
	for _, table := range TableNames {
		glog.Infof("Creating table %s...", table)
		d.createTable(table)
		glog.Infof("Created table %s.", table)
	}
	return nil
}

func (d *DynamoClient) tableExists(table string) bool {
	_, err := d.client.DescribeTable(context.Background(), &dynamodb.DescribeTableInput{
		TableName: &table})
	return err == nil
}

func (d *DynamoClient) createTable(table string) {
	keys := TableKeys[table]
	attrs := TableAttributes[table]
	streamSpec := TableStreamSpecs[table]
	_, err := d.client.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:            &table,
		KeySchema:            keys,
		AttributeDefinitions: attrs,
		// On-demand billing avoids needing to size provisioned capacity per
		// table. With 6 tables, provisioned mode at even the smallest useful
		// capacity (5 RCU/5 WCU each = 30/30 total) exceeds DynamoDB's
		// always-free tier of 25 RCU + 25 WCU combined per account/region,
		// which would incur a small ongoing charge. This app's traffic is low
		// enough that on-demand costs fractions of a cent per month.
		BillingMode:         types.BillingModePayPerRequest,
		StreamSpecification: &streamSpec})
	if err != nil {
		glog.Fatalf("Failed to create table %s %v", table, err)
	}
	glog.Infof("Created table %s.", table)
}
