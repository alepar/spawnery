package main

import "testing"

func TestStoreConfigFromEnv(t *testing.T) {
	cfg, err := storeConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg.Driver != "sqlite" || cfg.DSN != sqliteDefaultDSN {
		t.Fatalf("default = %+v err=%v", cfg, err)
	}
	pg := map[string]string{"CP_STORE_DRIVER": "postgres", "CP_STORE_DSN": "postgres://u:p@h/db"}
	cfg, err = storeConfigFromEnv(func(k string) string { return pg[k] })
	if err != nil || cfg.Driver != "postgres" || cfg.DSN != "postgres://u:p@h/db" {
		t.Fatalf("pg = %+v err=%v", cfg, err)
	}
	if _, err := storeConfigFromEnv(func(k string) string { return map[string]string{"CP_STORE_DRIVER": "postgres"}[k] }); err == nil {
		t.Fatal("postgres without DSN must error")
	}
	cfg, err = storeConfigFromEnv(func(k string) string { return map[string]string{"CP_STORE_DSN": "file:other.db"}[k] })
	if err != nil || cfg.Driver != "sqlite" || cfg.DSN != "file:other.db" {
		t.Fatalf("custom sqlite = %+v err=%v", cfg, err)
	}
}
