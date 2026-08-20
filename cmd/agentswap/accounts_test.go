package main

import (
	"testing"

	"github.com/bojieli/agentswap/internal/store"
)

func anthropicAndOpenAI() []*store.Account {
	return []*store.Account{
		{Lane: store.LaneAnthropic, Kind: store.KindOAuth},
		{Lane: store.LaneOpenAI, Kind: store.KindOAuth},
	}
}

// The README documents `agentswap import --id work` as the way to name the
// second account you pool, and then shows it in `status` as "work".
func TestImportUsesTheRequestedIDVerbatim(t *testing.T) {
	found := []*store.Account{{Lane: store.LaneAnthropic, Kind: store.KindOAuth}}
	nameImports(found, "work", "", map[string]bool{})

	if found[0].ID != "work" {
		t.Errorf("id = %q, want %q", found[0].ID, "work")
	}
	if found[0].Label != "work" {
		t.Errorf("label = %q, want it to default to the id", found[0].Label)
	}
}

// Two credentials from one import cannot share an id, so there the lane
// prefix is the only thing that keeps them apart.
func TestImportDisambiguatesTwoLanes(t *testing.T) {
	found := anthropicAndOpenAI()
	nameImports(found, "work", "", map[string]bool{})

	if found[0].ID != "anthropic-work" {
		t.Errorf("anthropic id = %q, want %q", found[0].ID, "anthropic-work")
	}
	if found[1].ID != "openai-work" {
		t.Errorf("openai id = %q, want %q", found[1].ID, "openai-work")
	}
}

func TestImportDisambiguatesTwoCredentialsInOneLaneWithRequestedID(t *testing.T) {
	found := []*store.Account{
		{Lane: store.LaneAnthropic, Kind: store.KindOAuth},
		{Lane: store.LaneAnthropic, Kind: store.KindAPIKey},
	}
	nameImports(found, "work", "", map[string]bool{})

	if found[0].ID != "anthropic-work" {
		t.Errorf("first anthropic id = %q, want anthropic-work", found[0].ID)
	}
	if found[1].ID != "anthropic-work-1" {
		t.Errorf("second anthropic id = %q, want anthropic-work-1", found[1].ID)
	}
}

func TestImportWithoutAnIDNumbersPerLane(t *testing.T) {
	found := anthropicAndOpenAI()
	nameImports(found, "", "", map[string]bool{})

	if found[0].ID != "anthropic-1" {
		t.Errorf("anthropic id = %q, want %q", found[0].ID, "anthropic-1")
	}
	if found[1].ID != "openai-1" {
		t.Errorf("openai id = %q, want %q", found[1].ID, "openai-1")
	}
}

// Importing a second login must not overwrite the first, which is the whole
// point of pooling.
func TestImportSkipsIDsAlreadyInThePool(t *testing.T) {
	found := []*store.Account{{Lane: store.LaneAnthropic, Kind: store.KindOAuth}}
	nameImports(found, "", "", map[string]bool{"anthropic-1": true, "anthropic-2": true})

	if found[0].ID != "anthropic-3" {
		t.Errorf("id = %q, want the first free slot %q", found[0].ID, "anthropic-3")
	}
}

func TestImportLabels(t *testing.T) {
	single := []*store.Account{{Lane: store.LaneAnthropic}}
	nameImports(single, "", "Personal", map[string]bool{})
	if single[0].Label != "Personal" {
		t.Errorf("label = %q, want %q", single[0].Label, "Personal")
	}

	// One label across two lanes would make `agentswap status` ambiguous.
	both := anthropicAndOpenAI()
	nameImports(both, "", "Personal", map[string]bool{})
	if both[0].Label != "Personal (anthropic)" {
		t.Errorf("label = %q, want it qualified by lane", both[0].Label)
	}
	if both[1].Label != "Personal (openai)" {
		t.Errorf("label = %q, want it qualified by lane", both[1].Label)
	}
}

// nameImports assigns ids in one pass, so it has to avoid colliding with the
// names it has already handed out in that same pass.
func TestImportDoesNotCollideWithinOneRun(t *testing.T) {
	found := []*store.Account{
		{Lane: store.LaneAnthropic},
		{Lane: store.LaneAnthropic},
	}
	nameImports(found, "", "", map[string]bool{})

	if found[0].ID == found[1].ID {
		t.Errorf("both accounts got id %q", found[0].ID)
	}
}

func TestImportNamesNewOverrideAfterKnownSubscription(t *testing.T) {
	found := []*store.Account{
		{ID: "anthropic-1", Label: "anthropic-1", Lane: store.LaneAnthropic},
		{Lane: store.LaneAnthropic},
	}
	nameImports(found, "", "", map[string]bool{"anthropic-1": true})

	if found[0].ID != "anthropic-1" {
		t.Errorf("known account was renamed to %q", found[0].ID)
	}
	if found[1].ID != "anthropic-2" {
		t.Errorf("new override id = %q, want anthropic-2", found[1].ID)
	}
}
