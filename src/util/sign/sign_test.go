package sign

import (
	"testing"
)

func TestGet(t *testing.T) {
	// Test with map input
	t.Run("Map input", func(t *testing.T) {
		data := map[string]interface{}{
			"name":  "test",
			"value": 123,
			"flag":  true,
		}
		bizKey := "testKey"

		sign, err := Get(data, bizKey)

		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}

		if sign == "" {
			t.Error("Expected non-empty signature, got empty string")
		}

		// Verify the same input produces the same signature
		sign2, err := Get(data, bizKey)
		if err != nil {
			t.Errorf("Expected no error on second call, got %v", err)
		}

		if sign != sign2 {
			t.Errorf("Expected consistent signatures, got %s and %s", sign, sign2)
		}
	})

	// Test with struct input
	t.Run("Struct input", func(t *testing.T) {
		type TestStruct struct {
			Name  string `json:"name"`
			Value int    `json:"value"`
			Flag  bool   `json:"flag"`
		}

		data := TestStruct{
			Name:  "test",
			Value: 123,
			Flag:  true,
		}
		bizKey := "testKey"

		sign, err := Get(data, bizKey)

		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}

		if sign == "" {
			t.Error("Expected non-empty signature, got empty string")
		}
	})

	// Test with invalid input type
	t.Run("Invalid input type", func(t *testing.T) {
		data := "invalid input"
		bizKey := "testKey"

		_, err := Get(data, bizKey)

		if err == nil {
			t.Error("Expected error for invalid input type, got nil")
		}
	})

	// Test with nil values and signature field
	t.Run("Map with nil values and signature field", func(t *testing.T) {
		data := map[string]interface{}{
			"name":      "test",
			"value":     nil,
			"signature": "should_be_ignored",
		}
		bizKey := "testKey"

		sign, err := Get(data, bizKey)

		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}

		if sign == "" {
			t.Error("Expected non-empty signature, got empty string")
		}

		// Create a similar map without the nil and signature fields
		dataCompare := map[string]interface{}{
			"name": "test",
		}

		signCompare, err := Get(dataCompare, bizKey)
		if err != nil {
			t.Errorf("Expected no error on comparison, got %v", err)
		}

		if sign != signCompare {
			t.Errorf("Expected signatures to match after ignoring nil and signature fields, got %s and %s", sign, signCompare)
		}
	})

	// Test with real-world order data
	t.Run("Real-world order data", func(t *testing.T) {
		data := map[string]interface{}{
			"amount":       150.00,
			"notify_url":   "https://oabetacb.ipoasis.cn/api/payment/callback/epusdt/success_pay",
			"order_id":     "250425001543182779",
			"redirect_url": "https://beta.ipoasis.io/dashboard/proxy/residential-proxies\\u0026payStatus=success&payType=purchase-residential-proxies",
		}
		bizKey := "reversegame" // Replace with appropriate key for testing

		sign, err := Get(data, bizKey)

		if err != nil {
			t.Errorf("Expected no error, got %v", err)
		}

		if sign == "" {
			t.Error("Expected non-empty signature, got empty string")
		}

		// Print the signature result
		t.Logf("Signature result for real-world data: %s", sign)

		if err != nil {
			t.Errorf("Expected no error when getting raw string, got %v", err)
		}

		// Verify the same input produces the same signature
		sign2, err := Get(data, bizKey)
		if err != nil {
			t.Errorf("Expected no error on second call, got %v", err)
		}

		if sign != sign2 {
			t.Errorf("Expected consistent signatures, got %s and %s", sign, sign2)
		}
	})
}
