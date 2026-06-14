package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// aliceCtx returns an authenticated context for the "alice" owner.
func aliceCtx() context.Context { return auth.WithOwner(context.Background(), "alice") }

// bobCtx returns an authenticated context for the "bob" owner.
func bobCtx() context.Context { return auth.WithOwner(context.Background(), "bob") }

// noAuthCtx returns an unauthenticated context (no owner set).
func noAuthCtx() context.Context { return context.Background() }

// --- CreateProfile ---------------------------------------------------------

func TestCreateProfile_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	resp, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "My Profile"}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if resp.Msg.ProfileId == "" {
		t.Error("expected non-empty profile_id")
	}
	if resp.Msg.Version != 1 {
		t.Errorf("expected version 1, got %d", resp.Msg.Version)
	}
}

func TestCreateProfile_EmptyName_Rejected(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: ""}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument on empty name, got %v", err)
	}
}

func TestCreateProfile_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.CreateProfile(noAuthCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "X"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

// --- GetProfile ------------------------------------------------------------

func TestGetProfile_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "My Profile"}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := cr.Msg.ProfileId

	gr, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if gr.Msg.Profile.ProfileId != profileID {
		t.Errorf("profile_id mismatch: got %q", gr.Msg.Profile.ProfileId)
	}
	if gr.Msg.Profile.Name != "My Profile" {
		t.Errorf("name mismatch: got %q", gr.Msg.Profile.Name)
	}
}

func TestGetProfile_NotFound(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: "no-such"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestGetProfile_OwnerIsolation(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "Alice's Profile"}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := cr.Msg.ProfileId

	// Bob should get NotFound for Alice's profile (don't leak existence).
	_, err = s.GetProfile(bobCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for bob, got %v", err)
	}
}

func TestGetProfile_Unauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)

	_, err := s.GetProfile(noAuthCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: "any"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

// --- ListProfiles ----------------------------------------------------------

func TestListProfiles_OwnerScoped(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Alice creates 2 profiles; Bob creates 1.
	for _, name := range []string{"A1", "A2"} {
		if _, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: name})); err != nil {
			t.Fatalf("CreateProfile alice %s: %v", name, err)
		}
	}
	if _, err := s.CreateProfile(bobCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "B1"})); err != nil {
		t.Fatalf("CreateProfile bob: %v", err)
	}

	aliceList, err := s.ListProfiles(aliceCtx(), connect.NewRequest(&cpv1.ListProfilesRequest{}))
	if err != nil {
		t.Fatalf("ListProfiles alice: %v", err)
	}
	if len(aliceList.Msg.Profiles) != 2 {
		t.Errorf("expected 2 alice profiles, got %d", len(aliceList.Msg.Profiles))
	}

	bobList, err := s.ListProfiles(bobCtx(), connect.NewRequest(&cpv1.ListProfilesRequest{}))
	if err != nil {
		t.Fatalf("ListProfiles bob: %v", err)
	}
	if len(bobList.Msg.Profiles) != 1 {
		t.Errorf("expected 1 bob profile, got %d", len(bobList.Msg.Profiles))
	}
}

// --- UpdateProfile ---------------------------------------------------------

func TestUpdateProfile_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "Old Name"}))
	profileID := cr.Msg.ProfileId

	ur, err := s.UpdateProfile(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "New Name",
	}))
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if ur.Msg.Version != 2 {
		t.Errorf("expected version 2, got %d", ur.Msg.Version)
	}

	gr, _ := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if gr.Msg.Profile.Name != "New Name" {
		t.Errorf("expected 'New Name', got %q", gr.Msg.Profile.Name)
	}
}

func TestUpdateProfile_CAS_Conflict(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	// Advance version.
	if _, err := s.UpdateProfile(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "V2",
	})); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Stale expected_version → Aborted.
	_, err := s.UpdateProfile(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "V3 stale",
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("expected Aborted on CAS conflict, got %v", err)
	}
}

func TestUpdateProfile_OwnerIsolation(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "Alice's"}))
	profileID := cr.Msg.ProfileId

	_, err := s.UpdateProfile(bobCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "Bob hijack",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for bob rename, got %v", err)
	}
}

func TestUpdateProfile_EmptyName_Rejected(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	_, err := s.UpdateProfile(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected InvalidArgument on empty name, got %v", err)
	}
}

// --- DeleteProfile ---------------------------------------------------------

func TestDeleteProfile_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "Delete Me"}))
	profileID := cr.Msg.ProfileId

	_, err := s.DeleteProfile(aliceCtx(), connect.NewRequest(&cpv1.DeleteProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}

	_, err = s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestDeleteProfile_OwnerIsolation(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "Alice's"}))
	profileID := cr.Msg.ProfileId

	_, err := s.DeleteProfile(bobCtx(), connect.NewRequest(&cpv1.DeleteProfileRequest{ProfileId: profileID}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("expected NotFound for bob delete, got %v", err)
	}
}

// --- AddProfileEntry -------------------------------------------------------

func TestAddProfileEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	aer, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:      "my-skill",
			Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
			CatalogId: "alice/myskill",
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v", err)
	}
	if aer.Msg.EntryId == "" {
		t.Error("expected non-empty entry_id")
	}
	if aer.Msg.Version != 2 {
		t.Errorf("expected version 2, got %d", aer.Msg.Version)
	}

	gr, _ := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if len(gr.Msg.Profile.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(gr.Msg.Profile.Entries))
	}
	e := gr.Msg.Profile.Entries[0]
	if e.Name != "my-skill" || e.Kind != cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestAddProfileEntry_Validation(t *testing.T) {
	s, _, _ := newTestServer(t)
	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	tests := []struct {
		name string
		req  *cpv1.AddProfileEntryRequest
	}{
		{
			name: "unspecified_kind",
			req: &cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind: cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED, Name: "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF, CatalogId: "a/b",
				},
			},
		},
		{
			name: "unspecified_source",
			req: &cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind:   cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
					Name:   "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_UNSPECIFIED,
				},
			},
		},
		{
			name: "empty_name",
			req: &cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
					Name:      "",
					Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
					CatalogId: "a/b",
				},
			},
		},
		{
			name: "catalog_ref_without_catalog_id",
			req: &cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind:   cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
					Name:   "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
					// catalog_id missing
				},
			},
		},
		{
			name: "custom_without_inline",
			req: &cpv1.AddProfileEntryRequest{
				ProfileId: profileID, ExpectedVersion: 1,
				Entry: &cpv1.ProfileEntry{
					Kind:   cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
					Name:   "x",
					Source: cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
					// custom_inline missing
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(tc.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestAddProfileEntry_CAS_Conflict(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	// Advance version.
	if _, err := s.UpdateProfile(aliceCtx(), connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId: profileID, ExpectedVersion: 1, Name: "P2",
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Stale expected_version.
	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1, // stale
		Entry: &cpv1.ProfileEntry{
			Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:      "sk",
			Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
			CatalogId: "a/b",
		},
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("expected Aborted on CAS conflict, got %v", err)
	}
}

// --- RemoveProfileEntry ----------------------------------------------------

func TestRemoveProfileEntry_Happy(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	aer, _ := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:      "sk",
			Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
			CatalogId: "a/b",
		},
	}))
	entryID := aer.Msg.EntryId

	rer, err := s.RemoveProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.RemoveProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 2,
		EntryId:         entryID,
	}))
	if err != nil {
		t.Fatalf("RemoveProfileEntry: %v", err)
	}
	if rer.Msg.Version != 3 {
		t.Errorf("expected version 3, got %d", rer.Msg.Version)
	}

	gr, _ := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if len(gr.Msg.Profile.Entries) != 0 {
		t.Errorf("expected 0 entries after remove, got %d", len(gr.Msg.Profile.Entries))
	}
}

// --- AddProfileSecretRef / RemoveProfileSecretRef --------------------------

func TestAddRemoveProfileSecretRef(t *testing.T) {
	s, _, _ := newTestServer(t)

	cr, _ := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "P"}))
	profileID := cr.Msg.ProfileId

	asr, err := s.AddProfileSecretRef(aliceCtx(), connect.NewRequest(&cpv1.AddProfileSecretRefRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		SecretId:        "sec-abc",
	}))
	if err != nil {
		t.Fatalf("AddProfileSecretRef: %v", err)
	}
	if asr.Msg.Version != 2 {
		t.Errorf("expected version 2, got %d", asr.Msg.Version)
	}

	gr, _ := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if len(gr.Msg.Profile.SecretIds) != 1 || gr.Msg.Profile.SecretIds[0] != "sec-abc" {
		t.Errorf("unexpected secret_ids: %v", gr.Msg.Profile.SecretIds)
	}

	rsr, err := s.RemoveProfileSecretRef(aliceCtx(), connect.NewRequest(&cpv1.RemoveProfileSecretRefRequest{
		ProfileId:       profileID,
		ExpectedVersion: 2,
		SecretId:        "sec-abc",
	}))
	if err != nil {
		t.Fatalf("RemoveProfileSecretRef: %v", err)
	}
	if rsr.Msg.Version != 3 {
		t.Errorf("expected version 3, got %d", rsr.Msg.Version)
	}

	gr2, _ := s.GetProfile(aliceCtx(), connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if len(gr2.Msg.Profile.SecretIds) != 0 {
		t.Errorf("expected 0 secret_ids after remove, got %d", len(gr2.Msg.Profile.SecretIds))
	}
}
