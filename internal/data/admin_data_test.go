package data

import (
	"strings"
	"testing"

	"ai-business-service/internal/conf"
)

func TestNewAdminDataRejectsNonStagingBeforeConnecting(t *testing.T) {
	config := &conf.Data{Mongo: &conf.Data_Mongo{
		Uri:                  "mongodb://127.0.0.1:27017/?replicaSet=rs0",
		Database:             "cling_main",
		ReplicaSet:           "rs0",
		TransactionsRequired: true,
	}}
	data, cleanup, err := NewAdminData(config)
	if cleanup != nil {
		cleanup()
	}
	if data != nil {
		t.Fatal("NewAdminData() returned data for local Mongo configuration")
	}
	if err == nil || !strings.Contains(err.Error(), "mongo database") {
		t.Fatalf("NewAdminData() error = %v, want staging database validation error", err)
	}
}
