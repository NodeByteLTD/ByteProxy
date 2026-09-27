package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthRequiredWithoutKeyFails(t *testing.T) {
	t.Setenv("REQUIRE_AUTH_FOR_PROXY", "true")
	t.Setenv("PROXY_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when proxy auth is required without a key")
	}
	t.Setenv("PROXY_API_KEY", "k")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaults(t *testing.T) {
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 3420 || s.UpdateRepo != "NodeByteLTD/ByteProxy" || !s.StrictTLS || s.DiscordGlobalRPS != 50 {
		t.Fatalf("unexpected defaults %+v", s)
	}
}

func TestDotEnvDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	os.WriteFile(file, []byte("# comment\nBP_TEST_A=\"from file\"\nexport BP_TEST_B=b\nBP_TEST_C=file\n"), 0o600)
	t.Setenv("BP_TEST_C", "process")
	os.Unsetenv("BP_TEST_A")
	os.Unsetenv("BP_TEST_B")
	t.Cleanup(func() { os.Unsetenv("BP_TEST_A"); os.Unsetenv("BP_TEST_B") })
	LoadDotEnv(file)
	if os.Getenv("BP_TEST_A") != "from file" || os.Getenv("BP_TEST_B") != "b" || os.Getenv("BP_TEST_C") != "process" {
		t.Fatalf("A=%q B=%q C=%q", os.Getenv("BP_TEST_A"), os.Getenv("BP_TEST_B"), os.Getenv("BP_TEST_C"))
	}
}
