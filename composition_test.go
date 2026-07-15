package main

import (
	"context"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"gopkg.in/yaml.v3"
)

func TestNewService_EmbedsBase(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
}

func TestLoadConfigurationUsesSettingsPassword(t *testing.T) {
	svc := NewService()
	svc.Password = "settings-secret"
	svc.RequirePass = true
	if err := svc.LoadConfiguration(context.Background(), nil); err != nil {
		t.Fatalf("LoadConfiguration: %v", err)
	}
	if svc.redisPassword != "settings-secret" {
		t.Fatalf("redisPassword = %q", svc.redisPassword)
	}
}

func TestLoadConfigurationRequiresPasswordWhenEnabled(t *testing.T) {
	svc := NewService()
	svc.RequirePass = true
	if err := svc.LoadConfiguration(context.Background(), nil); err == nil {
		t.Fatal("require-pass without a password succeeded")
	}
}

func TestRuntimeConfigurationOverridesSettingsPassword(t *testing.T) {
	svc := NewService()
	svc.Password = "settings-secret"
	conf := &basev0.Configuration{Infos: []*basev0.ConfigurationInformation{{
		Name:                "redis",
		ConfigurationValues: []*basev0.ConfigurationValue{{Key: "REDIS_PASSWORD", Value: "runtime-secret"}},
	}}}
	if err := svc.LoadConfiguration(context.Background(), conf); err != nil {
		t.Fatalf("LoadConfiguration: %v", err)
	}
	if svc.redisPassword != "runtime-secret" {
		t.Fatalf("redisPassword = %q, want runtime override", svc.redisPassword)
	}
}

func TestConnectionStringEscapesPassword(t *testing.T) {
	svc := NewService()
	svc.redisPassword = "p@ss:/ word"
	connection := svc.createConnectionString(context.Background(), "localhost:6379")
	if strings.Contains(connection, "p@ss:/ word") || connection != "redis://:p%40ss%3A%2F%20word@localhost:6379" {
		t.Fatalf("connection string = %q", connection)
	}
}

func TestRedisDockerCommandKeepsPasswordOutOfArgv(t *testing.T) {
	password := `secret with spaces`
	args := redisDockerCommand()
	if strings.Contains(strings.Join(args, " "), password) {
		t.Fatal("redis password leaked into Docker process argv")
	}
	if !strings.Contains(strings.Join(args, " "), "$REDIS_PASSWORD") {
		t.Fatalf("Docker command does not expand REDIS_PASSWORD: %q", args)
	}
}

func TestSettings_YAMLRoundTrip(t *testing.T) {
	src := []byte(`
password: "hunter2"
require-pass: true
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if s.Password != "hunter2" {
		t.Errorf("Password: got %q", s.Password)
	}
	if !s.RequirePass {
		t.Error("RequirePass not populated")
	}
}
