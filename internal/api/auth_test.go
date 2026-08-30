package api

import (
	"testing"
	"time"
)

// A lockout after a handful of wrong answers is what stops online guessing:
// bcrypt makes each attempt slow, but not slow enough to be a defence on its
// own.
func TestLoginThrottleLocksAfterRepeatedFailures(t *testing.T) {
	throttle := newLoginThrottle()
	const key = "email:someone@example.com"

	for i := 0; i < maxFailedAttempts-1; i++ {
		throttle.fail(key)
		if locked, _ := throttle.locked(key); locked {
			t.Fatalf("locked after only %d failures", i+1)
		}
	}

	throttle.fail(key)
	locked, wait := throttle.locked(key)
	if !locked {
		t.Fatalf("not locked after %d failures", maxFailedAttempts)
	}
	if wait <= 0 || wait > lockoutDuration {
		t.Errorf("lockout is %s, want up to %s", wait, lockoutDuration)
	}
}

// Getting it right clears the slate, so a forgetful operator who remembers on
// the fourth try is not punished for the next quarter of an hour.
func TestSuccessClearsAPartialLockout(t *testing.T) {
	throttle := newLoginThrottle()
	const key = "email:someone@example.com"

	throttle.fail(key)
	throttle.fail(key)
	throttle.succeed(key)

	for i := 0; i < maxFailedAttempts-1; i++ {
		throttle.fail(key)
	}
	if locked, _ := throttle.locked(key); locked {
		t.Error("the earlier failures were still being counted after a success")
	}
}

// Identities are counted apart, so one account being attacked does not lock
// out everyone else.
func TestThrottleIsPerIdentity(t *testing.T) {
	throttle := newLoginThrottle()

	for i := 0; i < maxFailedAttempts; i++ {
		throttle.fail("email:victim@example.com")
	}

	if locked, _ := throttle.locked("email:victim@example.com"); !locked {
		t.Fatal("the attacked account was not locked")
	}
	if locked, _ := throttle.locked("email:bystander@example.com"); locked {
		t.Error("an unrelated account was locked out too")
	}
	if locked, _ := throttle.locked("addr:203.0.113.9"); locked {
		t.Error("an unrelated address was locked out too")
	}
}

// Failures older than the window are forgotten, so a wrong password last week
// does not count towards a lockout today.
func TestOldFailuresExpire(t *testing.T) {
	throttle := newLoginThrottle()
	const key = "email:someone@example.com"

	throttle.mu.Lock()
	throttle.records[key] = &attemptRecord{
		failures: maxFailedAttempts - 1,
		first:    time.Now().Add(-2 * lockoutWindow),
	}
	throttle.mu.Unlock()

	// One more failure would lock it, were the old ones still counted.
	throttle.fail(key)
	if locked, _ := throttle.locked(key); locked {
		t.Error("failures from outside the window were counted towards the lockout")
	}
}
