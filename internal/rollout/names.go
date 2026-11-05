package rollout

import "regexp"

// These patterns match the check constraints in the database schema, so bad input
// gets a precise error instead of a constraint violation.
var (
	fleetNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	deviceIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	platformPattern  = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	labelPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// MaxLabels bounds how many labels a fleet, device, or selector may have.
const MaxLabels = 32

// ValidName reports whether s is a valid fleet or rollout name.
func ValidName(s string) bool { return fleetNamePattern.MatchString(s) }

// ValidDeviceID reports whether s is a valid device ID.
func ValidDeviceID(s string) bool { return deviceIDPattern.MatchString(s) }

// ValidPlatform reports whether s has the form os/arch.
func ValidPlatform(s string) bool { return platformPattern.MatchString(s) }

// ValidUUID reports whether s is a UUID in canonical lowercase form. Rank keys
// hash the rollout ID's text, so only one spelling of an ID can be accepted.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

// ValidLabels reports whether every key and value is well formed and the set is
// within MaxLabels.
func ValidLabels(labels map[string]string) bool {
	if len(labels) > MaxLabels {
		return false
	}
	for k, v := range labels {
		if !labelPattern.MatchString(k) || !labelPattern.MatchString(v) {
			return false
		}
	}
	return true
}
