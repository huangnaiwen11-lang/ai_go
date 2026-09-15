package data

import (
	"strings"
	"testing"

	"ai-business-service/internal/conf"
)

func TestNewDataRejectsUnsafeMongoBeforeConnecting(t *testing.T) {
	config := &conf.Data{
		Mongo: &conf.Data_Mongo{
			Uri:                  "mongodb://127.0.0.1:27018/?replicaSet=rs0",
			Database:             "cling_main",
			ReplicaSet:           "rs0",
			TransactionsRequired: true,
		},
	}

	data, cleanup, err := NewData(config)
	if cleanup != nil {
		cleanup()
	}
	if data != nil {
		t.Fatal("NewData() returned data for an unsafe MongoDB URI")
	}
	if err == nil || !strings.Contains(err.Error(), "mongo port") {
		t.Fatalf("NewData() error = %v, want MongoDB port isolation error", err)
	}
}
