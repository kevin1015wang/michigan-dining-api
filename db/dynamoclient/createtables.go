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
		desc, err := d.describeTable(table)
		if err != nil {
			glog.Infof("Table %s does not exist. Creating now...", table)
			d.createTable(table)
			continue
		}
		glog.Infof("Table %s exists.", table)
		d.ensureOnDemandBilling(table, desc)
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

func (d *DynamoClient) describeTable(table string) (*types.TableDescription, error) {
	out, err := d.client.DescribeTable(context.Background(), &dynamodb.DescribeTableInput{
		TableName: &table})
	if err != nil {
		return nil, err
	}
	return out.Table, nil
}

// ensureOnDemandBilling migrates a table that was created before this app
// switched to on-demand billing (see createTable) off whatever small fixed
// capacity it originally got provisioned with. A table stuck on a leftover
// 1 RCU/1 WCU can throttle hard under any write burst bigger than what it
// was sized for, silently dropping writes that exhaust their retries.
func (d *DynamoClient) ensureOnDemandBilling(table string, desc *types.TableDescription) {
	if desc.BillingModeSummary != nil && desc.BillingModeSummary.BillingMode == types.BillingModePayPerRequest {
		return
	}
	glog.Infof("Table %s is not on on-demand billing, switching it now...", table)
	_, err := d.client.UpdateTable(context.Background(), &dynamodb.UpdateTableInput{
		TableName:   &table,
		BillingMode: types.BillingModePayPerRequest})
	if err != nil {
		glog.Errorf("Failed to switch table %s to on-demand billing: %v", table, err)
	}
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
