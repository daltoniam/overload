package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvFileRoundTripAndPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overload.env")
	if err := writeEnvFile(path, map[string]string{"DATABASE_URL": "postgres://from-file", "OVERLOAD_UI_USER": "admin"}, configOrder); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", info.Mode().Perm(), err)
	}
	t.Setenv("DATABASE_URL", "postgres://from-env")
	t.Setenv("OVERLOAD_UI_USER", "")
	_ = os.Unsetenv("OVERLOAD_UI_USER")
	if err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DATABASE_URL") != "postgres://from-env" || os.Getenv("OVERLOAD_UI_USER") != "admin" {
		t.Fatalf("env DATABASE_URL=%q OVERLOAD_UI_USER=%q", os.Getenv("DATABASE_URL"), os.Getenv("OVERLOAD_UI_USER"))
	}
	if err := loadConfig(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	_ = os.WriteFile(path, []byte("# comment\n\nA=1\nB = \"two words\"\nC='x=y'\n"), 0o600)
	values, err := readEnvFile(path)
	if err != nil || values["A"] != "1" || values["B"] != "two words" || values["C"] != "x=y" {
		t.Fatalf("%v %v", values, err)
	}
	_ = os.WriteFile(path, []byte("not a pair\n"), 0o600)
	if _, err := readEnvFile(path); err == nil {
		t.Fatal("accepted malformed line")
	}
}

func TestConfigureNative(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"initdb", "postgres"} {
		_ = os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755)
	}
	inst := &installation{home: t.TempDir(), label: "dev.overload.test"}
	if err := inst.configure(18095, "native", "", bin); err != nil {
		t.Fatal(err)
	}
	if inst.values["OVERLOAD_POSTGRES_BIN"] != bin || !strings.HasPrefix(inst.values["DATABASE_URL"], "postgres://overload:") || inst.values["OVERLOAD_ADDR"] != "127.0.0.1:18095" || len(inst.values["OVERLOAD_UI_PASSWORD"]) != 48 {
		t.Fatalf("%v", inst.values)
	}
	if err := (&installation{home: t.TempDir()}).configure(18095, "url", "", ""); err == nil {
		t.Fatal("url mode without --database-url")
	}
}

func TestInstallRejectsUnsafeInput(t *testing.T) {
	if err := writeEnvFile(filepath.Join(t.TempDir(), "x.env"), map[string]string{"DATABASE_URL": "postgres://x\nOVERLOAD_UI_INSECURE=1"}, configOrder); err == nil {
		t.Fatal("line break in a value would inject another setting")
	}
	for _, label := range []string{"../evil", "dev.overload/../x", "Dev.Overload", "nodots"} {
		if _, err := openInstallation(t.TempDir(), label); err == nil {
			t.Errorf("accepted label %q", label)
		}
	}
	if _, err := openInstallation(t.TempDir(), "dev.overload.test-1"); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationHomeFollowsOverloadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OVERLOAD_CONFIG", filepath.Join(home, "overload.env"))
	inst, err := openInstallation("", defaultLabel)
	if err != nil || inst.home != home || inst.config != filepath.Join(home, "overload.env") {
		t.Fatalf("install home %+v %v", inst, err)
	}
}
