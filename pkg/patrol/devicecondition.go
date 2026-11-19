package patrol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"siapp/pkg/platform/apperr"
)

// The condition of the guard's phone when it answered: battery, network, GPS and so on, sent by the app as
// a JSON object (Phase 10.9, U13; Phase 10.12 records it for a patrol too).
//
// ⚠ It is evidence, not a gate. The server never refuses a scan because of what it says (a phone that
// reports mock_location is stored as it reported and the scan is accepted: the DANRU decides what to make
// of it), and never trusts it (the app reports it, so it proves nothing by itself). What it does insist on
// is the shape, so that what is stored can be read: an object, not too large, and the known keys with their
// own types.

// maxDeviceConditionBytes is the largest device condition accepted, as sent.
const maxDeviceConditionBytes = 4096

// maxDeviceTextLength is the longest text value of a known key, in characters.
const maxDeviceTextLength = 64

// ReasonDeviceConditionInvalid is the reason of a device condition that is refused.
const ReasonDeviceConditionInvalid = "DEVICE_CONDITION_INVALID"

// networkTypes are the values of network_type.
var networkTypes = []string{"wifi", "cellular", "none", "unknown"}

// parseDeviceCondition checks a device condition and returns it as it will be stored; nil when none was
// sent (absent, or null). field names it in the error (device_condition, or actions[2].device_condition).
//
// The known keys are checked only when present: battery_level a number from 0 to 100; is_charging,
// gps_enabled and mock_location booleans; network_type one of wifi, cellular, none, unknown; os,
// os_version, device_model and app_version text of up to 64 characters. Keys it does not know are kept as
// they came: the phone's OS and the app will add fields over time, and refusing a scan for one would lose
// field evidence.
func parseDeviceCondition(raw json.RawMessage, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	fail := func(msg string) error {
		return apperr.ErrValidation.WithReason(ReasonDeviceConditionInvalid).WithFields(apperr.FieldError{
			Field: field, Code: "INVALID", Message: msg,
		})
	}
	if len(trimmed) > maxDeviceConditionBytes {
		return nil, fail(fmt.Sprintf("Maksimal %d byte.", maxDeviceConditionBytes))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, fail("Harus berupa objek JSON.")
	}
	for key, value := range obj {
		switch key {
		case "battery_level":
			var n float64
			if err := json.Unmarshal(value, &n); err != nil || n < 0 || n > 100 {
				return nil, fail("battery_level harus angka antara 0 dan 100.")
			}
		case "is_charging", "gps_enabled", "mock_location":
			var b bool
			if err := json.Unmarshal(value, &b); err != nil {
				return nil, fail(key + " harus true atau false.")
			}
		case "network_type":
			var s string
			if err := json.Unmarshal(value, &s); err != nil || !slices.Contains(networkTypes, s) {
				return nil, fail("network_type harus salah satu dari: wifi, cellular, none, unknown.")
			}
		case "os", "os_version", "device_model", "app_version":
			var s string
			if err := json.Unmarshal(value, &s); err != nil || utf8.RuneCountInString(s) > maxDeviceTextLength {
				return nil, fail(fmt.Sprintf("%s harus teks paling panjang %d karakter.", key, maxDeviceTextLength))
			}
		}
	}
	// Stored re-encoded, not as the client sent it: a lone surrogate escape (backslash-u d800) passes Go's
	// decoder but jsonb rejects it, and that insert error would reach the client as a 500. Marshal turns it
	// into U+FFFD. UseNumber keeps big numbers exact.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var clean map[string]any
	if err := dec.Decode(&clean); err != nil {
		return nil, fail("Harus berupa objek JSON.")
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return nil, fail("Harus berupa objek JSON.")
	}
	return out, nil
}
