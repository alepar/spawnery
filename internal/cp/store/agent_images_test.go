package store

import "testing"

func TestAgentImagesWiring(t *testing.T) {
	st := NewTestStore(t) // opens + applies all migrations, incl. 0005
	if st.AgentImages() == nil {
		t.Fatal("AgentImages() returned nil")
	}
}
