package store

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	authv1 "spawnery/gen/auth/v1"
)

func TestRevocationProtoCarriesRetentionAndReservesLegacyTokens(t *testing.T) {
	desc := (&authv1.RevocationEntry{}).ProtoReflect().Descriptor()
	if field := desc.Fields().ByName("token_ids"); field != nil {
		t.Fatal("legacy token_ids field remains active")
	}
	if !desc.ReservedRanges().Has(protoreflect.FieldNumber(4)) || !desc.ReservedNames().Has("token_ids") {
		t.Fatal("legacy token_ids field number/name are not reserved")
	}
	revokedTokens := desc.Fields().ByName("revoked_tokens")
	if revokedTokens == nil || revokedTokens.Number() != 6 || !revokedTokens.IsList() || revokedTokens.Message() == nil {
		t.Fatalf("revoked_tokens descriptor: %v", revokedTokens)
	}
	cutoff := desc.Fields().ByName("revoke_tokens_issued_before")
	if cutoff == nil || cutoff.Number() != 7 || cutoff.Kind() != protoreflect.Int64Kind {
		t.Fatalf("account cutoff descriptor: %v", cutoff)
	}
	tokenDesc := revokedTokens.Message()
	if tokenDesc.Fields().ByName("token_id") == nil || tokenDesc.Fields().ByName("retain_until") == nil {
		t.Fatalf("typed revoked token descriptor: %v", tokenDesc.Fields())
	}
}
