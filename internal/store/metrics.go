package store

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// LicenseKeyDecryptFailures counts how many DecryptLicenseKey calls have
// failed AEAD verification. A non-zero value usually means either:
//   - ciphertext corruption (DB tampering, partial disk write), or
//   - master key change without re-encrypting existing rows, or
//   - a bug in the AAD binding logic.
//
// Alert when this counter increments. Phase C deployment is unsafe while
// any failures are recorded.
var LicenseKeyDecryptFailures = promauto.NewCounter(prometheus.CounterOpts{
	Name: "keygate_license_key_decrypt_failures_total",
	Help: "Cumulative count of license_key_encrypted decrypt failures.",
})

// LicenseKeysUnencrypted is a gauge for the count of license rows where
// license_key_encrypted IS NULL but license_key is non-empty, plus the
// members' keys in the same state (seats.member_key). This is the
// number of rows that still need backfill before Phase B can flip the
// read path to encrypted-only.
//
// Updated on each backfill batch + once at startup; used as the deciding
// metric for "is it safe to enable Phase B?".
var LicenseKeysUnencrypted = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "keygate_license_keys_unencrypted",
	Help: "Number of license rows still requiring license_key_encrypted backfill.",
})

// licenseKeyDecryptFailuresAtomic is a goroutine-safe shadow of the
// counter, kept locally for tests that don't want to scrape Prometheus.
var licenseKeyDecryptFailuresAtomic atomic.Int64

func licenseKeyDecryptFailuresInc() {
	LicenseKeyDecryptFailures.Inc()
	licenseKeyDecryptFailuresAtomic.Add(1)
}

// LicenseKeyDecryptFailureCount returns the running atomic count for tests.
func LicenseKeyDecryptFailureCount() int64 {
	return licenseKeyDecryptFailuresAtomic.Load()
}
