package prom

import "testing"

func TestValues(t *testing.T) {
	t.Run("all series", func(t *testing.T) {
		payload := `{"status":"success","data":{"result":[{"value":[1700000000,"1"]},{"value":[1700000000,"1"]},{"value":[1700000000,"0"]}]}}`
		values, err := Values(payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(values) != 3 || values[0] != 1 || values[2] != 0 {
			t.Errorf("got %v, want [1 1 0]", values)
		}
	})

	t.Run("empty result", func(t *testing.T) {
		values, err := Values(`{"status":"success","data":{"result":[]}}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("got %v, want no values", values)
		}
	})

	t.Run("error status", func(t *testing.T) {
		if _, err := Values(`{"status":"error","errorType":"bad_data"}`); err == nil {
			t.Error("expected an error for a failed query status")
		}
	})

	t.Run("sample without a value", func(t *testing.T) {
		if _, err := Values(`{"status":"success","data":{"result":[{"value":[1700000000]}]}}`); err == nil {
			t.Error("expected an error for a sample without a value")
		}
	})
}
