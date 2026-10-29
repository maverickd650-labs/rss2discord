package controller

import (
	"errors"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

func TestSetReadyCondition_ClampsWebhookErrorMessage(t *testing.T) {
	webhookErr := errors.New(strings.Repeat("k", 10*maxConditionMessageBytes))
	fg := &v1alpha1.FeedGroup{}
	setReadyCondition(fg, webhookErr, nil)

	cond := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatal("expected a Ready condition")
	}
	if want := clampText(webhookErr.Error(), maxConditionMessageBytes); cond.Message != want {
		t.Fatalf("Ready message is %d bytes, want the %d-byte clamped webhook error", len(cond.Message), len(want))
	}
	if len(cond.Message) > maxConditionMessageBytes {
		t.Fatalf("Ready message is %d bytes, want at most %d", len(cond.Message), maxConditionMessageBytes)
	}
}
