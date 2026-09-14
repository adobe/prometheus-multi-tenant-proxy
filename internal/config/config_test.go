package config

// Config regression tests for the self-heal safety behaviors:
//   - the last-resort self-heal pod restart is OPT-IN: a nil (unset)
//     SelfHealPodRestart defaults to false.
//   - a non-fatal warning is logged when PodRestartGracePeriod is shorter
//     than StallGracePeriod (escalation may be premature).

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestSetDefaults_SelfHealPodRestart_DefaultsOff(t *testing.T) {
	cfg := &Config{}
	if err := setDefaults(cfg); err != nil {
		t.Fatalf("setDefaults returned error: %v", err)
	}
	if cfg.RemoteWrite.SelfHealPodRestart == nil {
		t.Fatalf("expected setDefaults to populate SelfHealPodRestart")
	}
	if *cfg.RemoteWrite.SelfHealPodRestart {
		t.Fatalf("expected the pod-restart escalation to default OFF (opt-in), got true")
	}
}

func TestSetDefaults_SelfHealPodRestart_ExplicitTruePreserved(t *testing.T) {
	enabled := true
	cfg := &Config{RemoteWrite: RemoteWriteConfig{SelfHealPodRestart: &enabled}}
	if err := setDefaults(cfg); err != nil {
		t.Fatalf("setDefaults returned error: %v", err)
	}
	if cfg.RemoteWrite.SelfHealPodRestart == nil || !*cfg.RemoteWrite.SelfHealPodRestart {
		t.Fatalf("expected an explicit true to be preserved through setDefaults")
	}
}

func TestValidate_WarnsWhenPodRestartGraceBelowStallGrace(t *testing.T) {
	var buf bytes.Buffer
	prevOut := logrus.StandardLogger().Out
	prevLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.WarnLevel)
	defer func() {
		logrus.SetOutput(prevOut)
		logrus.SetLevel(prevLevel)
	}()

	cfg := &Config{}
	if err := setDefaults(cfg); err != nil {
		t.Fatalf("setDefaults returned error: %v", err)
	}
	cfg.RemoteWrite.StallGracePeriod = 15 * time.Minute
	cfg.RemoteWrite.PodRestartGracePeriod = 1 * time.Minute // shorter than stall grace

	if err := validate(cfg); err != nil {
		t.Fatalf("validate must remain non-fatal, got error: %v", err)
	}
	if !strings.Contains(buf.String(), "premature") {
		t.Fatalf("expected a warning about premature escalation, got log: %q", buf.String())
	}
}

func TestValidate_NoWarnWhenPodRestartGraceAboveStallGrace(t *testing.T) {
	var buf bytes.Buffer
	prevOut := logrus.StandardLogger().Out
	prevLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.WarnLevel)
	defer func() {
		logrus.SetOutput(prevOut)
		logrus.SetLevel(prevLevel)
	}()

	cfg := &Config{}
	if err := setDefaults(cfg); err != nil {
		t.Fatalf("setDefaults returned error: %v", err)
	}
	cfg.RemoteWrite.StallGracePeriod = 15 * time.Minute
	cfg.RemoteWrite.PodRestartGracePeriod = 45 * time.Minute // >= stall grace

	if err := validate(cfg); err != nil {
		t.Fatalf("validate returned error: %v", err)
	}
	if strings.Contains(buf.String(), "premature") {
		t.Fatalf("did not expect a premature-escalation warning, got log: %q", buf.String())
	}
}
