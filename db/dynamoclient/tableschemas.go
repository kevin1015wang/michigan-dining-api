package dynamoclient

import (
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	DiningHallsTableName = "DiningHalls"
	ItemsTableName       = "Items"
	MenuTableName        = "Menus"
	FoodTableName        = "Foods"
	FoodStatsTableName   = "FoodStats"
	HeartsTableName      = "Hearts"
)

var (
	NameKey                    = "name"
	DiningHallDateMealKey      = "key"
	DateKey                    = "date"
	NameDateKey                = "key"
	FoodTableNameKey           = "key"
	MenuTableDiningHallMealKey = "diningHallMeal"
	FoodStatsDateKey           = "date"
	HeartsTableKey             = "key"
)

var (
	trueValue  = true
	falseValue = false
)

var (
	TableNames = []string{
		DiningHallsTableName,
		ItemsTableName,
		MenuTableName,
		FoodTableName,
		FoodStatsTableName,
		HeartsTableName}
	TableKeys = map[string][]types.KeySchemaElement{
		DiningHallsTableName: {
			{AttributeName: &NameKey, KeyType: types.KeyTypeHash}},
		ItemsTableName: {
			{AttributeName: &NameKey, KeyType: types.KeyTypeHash}},
		MenuTableName: {
			{AttributeName: &DateKey, KeyType: types.KeyTypeHash},
			{AttributeName: &MenuTableDiningHallMealKey, KeyType: types.KeyTypeRange}},
		FoodTableName: {
			{AttributeName: &FoodTableNameKey, KeyType: types.KeyTypeHash},
			{AttributeName: &DateKey, KeyType: types.KeyTypeRange}},
		FoodStatsTableName: {
			{AttributeName: &FoodStatsDateKey, KeyType: types.KeyTypeHash}},
		HeartsTableName: {
			{AttributeName: &HeartsTableKey, KeyType: types.KeyTypeHash}}}
	TableAttributes = map[string][]types.AttributeDefinition{
		DiningHallsTableName: {
			{AttributeName: &NameKey, AttributeType: types.ScalarAttributeTypeS}},
		ItemsTableName: {
			{AttributeName: &NameKey, AttributeType: types.ScalarAttributeTypeS}},
		MenuTableName: {
			{AttributeName: &DateKey, AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: &MenuTableDiningHallMealKey, AttributeType: types.ScalarAttributeTypeS}},
		FoodTableName: {
			{AttributeName: &FoodTableNameKey, AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: &DateKey, AttributeType: types.ScalarAttributeTypeS}},
		FoodStatsTableName: {
			{AttributeName: &FoodStatsDateKey, AttributeType: types.ScalarAttributeTypeS}},
		HeartsTableName: {
			{AttributeName: &HeartsTableKey, AttributeType: types.ScalarAttributeTypeS}}}
	TableStreamSpecs = map[string]types.StreamSpecification{
		// StreamViewType must be left unset when StreamEnabled is false; DynamoDB rejects the combination.
		DiningHallsTableName: {StreamEnabled: &falseValue},
		ItemsTableName:       {StreamEnabled: &falseValue},
		MenuTableName:        {StreamEnabled: &falseValue},
		FoodTableName:        {StreamEnabled: &falseValue},
		FoodStatsTableName:   {StreamEnabled: &falseValue},
		HeartsTableName:      {StreamEnabled: &trueValue, StreamViewType: types.StreamViewTypeNewImage},
	}
)
