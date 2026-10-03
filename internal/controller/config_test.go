package controller

import (
	"reflect"
	"strings"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"LWD_DATABASE_URL": "postgres://x",
		"LWD_API_TOKEN":    "tok",
		"HOME":             "/home/u",
	}
	get := func(k string) string { return env[k] }
	cfg, err := ConfigFromEnv(get)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{DatabaseURL: "postgres://x", APIToken: "tok", Listen: "127.0.0.1:7470", SecretKeyFile: "/home/u/.config/lwd/secret.key", BackupHour: 3}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg = %+v", cfg)
	}

	env["XDG_CONFIG_HOME"] = "/xdg"
	env["LWD_LISTEN"] = ":9000"
	env["LWD_INSECURE_REGISTRIES"] = "localhost:5000, reg.dev:5000 ,"
	cfg, _ = ConfigFromEnv(get)
	if cfg.SecretKeyFile != "/xdg/lwd/secret.key" || cfg.Listen != ":9000" || !reflect.DeepEqual(cfg.InsecureRegistries, []string{"localhost:5000", "reg.dev:5000"}) {
		t.Errorf("cfg = %+v", cfg)
	}
	env["LWD_SECRET_KEY_FILE"] = "/etc/lwd/key"
	if cfg, _ = ConfigFromEnv(get); cfg.SecretKeyFile != "/etc/lwd/key" {
		t.Errorf("key file = %s", cfg.SecretKeyFile)
	}

	env["LWD_BACKUP_HOUR"] = "23"
	if cfg, _ = ConfigFromEnv(get); cfg.BackupHour != 23 {
		t.Errorf("backup hour = %d", cfg.BackupHour)
	}
	for _, bad := range []string{"24", "-1", "3am"} {
		env["LWD_BACKUP_HOUR"] = bad
		if _, err := ConfigFromEnv(get); err == nil || !strings.Contains(err.Error(), "LWD_BACKUP_HOUR") {
			t.Errorf("LWD_BACKUP_HOUR=%s: err = %v", bad, err)
		}
	}
	delete(env, "LWD_BACKUP_HOUR")

	for _, missing := range []string{"LWD_API_TOKEN", "LWD_DATABASE_URL"} {
		saved := env[missing]
		delete(env, missing)
		if _, err := ConfigFromEnv(get); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: err = %v", missing, err)
		}
		env[missing] = saved
	}
}
