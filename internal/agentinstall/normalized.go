package agentinstall

// ValidNormalizedConfigKeys is the complete set of normalized config key names
// understood by the agentinstall engine. Any key in ConfigPayload.Normalized
// that is not in this set is rejected by the CP assembler as defense-in-depth
// (sp-nrzf.3.7). The engine itself logs unknown keys at apply time; the
// assembler fails loudly so the profile owner learns immediately.
var ValidNormalizedConfigKeys = map[string]bool{
	ckApprovalPosture: true,
	ckDisabledTools:   true,
	ckAllowedCommands: true,
	ckDeniedCommands:  true,
}
