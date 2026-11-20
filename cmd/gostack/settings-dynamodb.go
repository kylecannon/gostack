package main

import (
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
)

// dynamodbSettings maps the configuration onto the settings of the DynamoDB
// provider.
func dynamodbSettings(current *config.Settings) dynamodb.Settings {
	return dynamodb.Settings{
		ErrorProbability:      current.DynamoDBErrorProbability,
		ReadErrorProbability:  current.DynamoDBReadErrorProbability,
		WriteErrorProbability: current.DynamoDBWriteErrorProbability,
		ShareDB:               current.DynamoDBShareDB != 0,
		Host:                  current.LocalstackHost.String(),
		RemoveExpiredItems:    current.DynamoDBRemoveExpiredItems,
		DisableCompatPaths:    !current.CompatPaths,
	}
}
