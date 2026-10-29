package discord

import (
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// testWebhookURL is reused across cases below that don't care about a
// specific URL, just that cooldowns key off of one.
const testWebhookURL = "https://discord.com/api/webhooks/1/token"

func TestRateLimiter_ReserveNotCoolingWhenUnseen(t *testing.T) {
	l := NewRateLimiter()
	if remaining, cooling := l.reserve(testWebhookURL); cooling || remaining != 0 {
		t.Fatalf("expected no cooldown for an unseen webhook, got remaining=%v cooling=%v", remaining, cooling)
	}
}

func TestRateLimiter_CooldownThenReserve(t *testing.T) {
	l := NewRateLimiter()
	webhookURL := testWebhookURL

	l.cooldown(webhookURL, time.Minute)

	remaining, cooling := l.reserve(webhookURL)
	if !cooling {
		t.Fatal("expected the webhook to be cooling down immediately after cooldown()")
	}
	if remaining <= 0 || remaining > time.Minute {
		t.Fatalf("expected remaining in (0, 1m], got %v", remaining)
	}
}

func TestRateLimiter_CooldownIsPerWebhook(t *testing.T) {
	l := NewRateLimiter()
	a := "https://discord.com/api/webhooks/1/token-a"
	b := "https://discord.com/api/webhooks/2/token-b"

	l.cooldown(a, time.Minute)

	if _, cooling := l.reserve(b); cooling {
		t.Fatal("a different webhook must not be affected by another webhook's cooldown")
	}
	if _, cooling := l.reserve(a); !cooling {
		t.Fatal("expected the original webhook to still be cooling down")
	}
}

// TestRateLimiter_CooldownExpiresAfterRetryAfter exercises the real expiry
// transition -- cooling down, then not, once the full duration has actually
// elapsed -- using synctest's fake clock instead of TestRateLimiter_
// ExpiredCooldownIsPruned's approach of faking "already expired" with a
// negative duration. time.Sleep inside the bubble advances the fake clock
// instantly rather than pausing the test for a full minute.
func TestRateLimiter_CooldownExpiresAfterRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewRateLimiter()
		webhookURL := testWebhookURL

		l.cooldown(webhookURL, time.Minute)

		if _, cooling := l.reserve(webhookURL); !cooling {
			t.Fatal("expected the webhook to be cooling down immediately after cooldown()")
		}

		time.Sleep(time.Minute)

		if _, cooling := l.reserve(webhookURL); cooling {
			t.Fatal("expected the cooldown to have expired after the full retry-after duration")
		}
		if len(l.cooldowns) != 0 {
			t.Fatalf("expected the expired entry to be pruned, got %d entries", len(l.cooldowns))
		}
	})
}

func TestRateLimiter_ExpiredCooldownIsPruned(t *testing.T) {
	l := NewRateLimiter()
	webhookURL := testWebhookURL

	// A cooldown that has already elapsed (negative duration => expiry in
	// the past) must not report cooling, and must be pruned from the map so
	// a webhook that's never sent to again doesn't leak an entry forever.
	l.cooldown(webhookURL, -time.Second)

	if _, cooling := l.reserve(webhookURL); cooling {
		t.Fatal("expected an already-expired cooldown to not report cooling")
	}
	if len(l.cooldowns) != 0 {
		t.Fatalf("expected the expired entry to be pruned, got %d entries", len(l.cooldowns))
	}
}

func TestWebhookKey_HashesRatherThanStoringRawURL(t *testing.T) {
	l := NewRateLimiter()
	webhookURL := "https://discord.com/api/webhooks/1/super-secret-token"

	l.cooldown(webhookURL, time.Minute)

	for key := range l.cooldowns {
		if key == webhookURL {
			t.Fatal("expected the map key to be a hash, not the raw webhook URL")
		}
	}
	if got := webhookKey(webhookURL); len(got) != 64 {
		t.Fatalf("expected a 64-char hex-encoded SHA-256 digest, got %d chars: %q", len(got), got)
	}
}

func TestRateLimiter_CooldownNeverShortens(t *testing.T) {
	l := NewRateLimiter()

	l.cooldown(testWebhookURL, time.Minute)
	l.cooldown(testWebhookURL, time.Second)

	if remaining, cooling := l.reserve(testWebhookURL); !cooling || remaining <= 30*time.Second {
		t.Fatalf("a shorter later cooldown must not release the webhook early: remaining=%v cooling=%v", remaining, cooling)
	}

	l.cooldown(testWebhookURL, time.Hour)
	if remaining, _ := l.reserve(testWebhookURL); remaining <= 30*time.Minute {
		t.Fatalf("a longer later cooldown must extend it, remaining=%v", remaining)
	}
}

func TestRateLimiter_CooldownIsClampedToMax(t *testing.T) {
	for _, requested := range []time.Duration{1000 * time.Hour, time.Duration(math.MaxInt64)} {
		l := NewRateLimiter()
		l.cooldown(testWebhookURL, requested)

		remaining, cooling := l.reserve(testWebhookURL)
		if !cooling || remaining > maxCooldown || remaining < maxCooldown-time.Minute {
			t.Fatalf("cooldown(%v): remaining=%v cooling=%v, want it clamped to about %v", requested, remaining, cooling, maxCooldown)
		}
	}
}

func TestRateLimiter_ExcessiveCooldownCannotExtendAClampedOne(t *testing.T) {
	l := NewRateLimiter()
	l.cooldown(testWebhookURL, maxCooldown)
	l.cooldown(testWebhookURL, 5000*time.Hour)
	l.cooldown(testWebhookURL, time.Minute)

	if remaining, _ := l.reserve(testWebhookURL); remaining > maxCooldown || remaining < maxCooldown-time.Minute {
		t.Fatalf("remaining = %v, want about %v: neither a longer nor a shorter request may move it", remaining, maxCooldown)
	}
}

func TestRateLimiter_PaceIsClampedToMaxPaceWait(t *testing.T) {
	l := NewRateLimiter()
	l.pace(testWebhookURL, time.Hour)

	if d := l.paceDelay(testWebhookURL); d <= 0 || d > maxPaceWait {
		t.Fatalf("paceDelay = %v, want in (0, %v]", d, maxPaceWait)
	}
}
