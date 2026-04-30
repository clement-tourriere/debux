package picker

import (
	"bytes"
	"testing"
)

func TestPickTextFromReaderFiltersAndSelects(t *testing.T) {
	items := []Item{
		{Label: "gim/api-a", Value: "api-a"},
		{Label: "gim/webapp-internal-api-1", Value: "webapp-internal-api-1"},
		{Label: "gim/webapp-internal-api-2", Value: "webapp-internal-api-2"},
	}

	var out bytes.Buffer
	selected, err := pickTextFromReader("Select", items, bytes.NewBufferString("internal\n2\n"), &out)
	if err != nil {
		t.Fatalf("pickTextFromReader returned error: %v", err)
	}
	if selected != "webapp-internal-api-2" {
		t.Fatalf("selected = %q", selected)
	}
}

func TestPickTextFromReaderSingleMatchOnEnter(t *testing.T) {
	items := []Item{{Label: "gim/api-a", Value: "api-a"}}
	var out bytes.Buffer
	selected, err := pickTextFromReader("Select", items, bytes.NewBufferString("\n"), &out)
	if err != nil {
		t.Fatalf("pickTextFromReader returned error: %v", err)
	}
	if selected != "api-a" {
		t.Fatalf("selected = %q", selected)
	}
}
