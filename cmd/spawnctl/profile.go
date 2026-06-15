package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// profileClient is the subset of the cp.v1 client that the profile commands drive.
// Narrowed to an interface so the commands are unit-testable with a fake client.
type profileClient interface {
	CreateProfile(context.Context, *connect.Request[cpv1.CreateProfileRequest]) (*connect.Response[cpv1.CreateProfileResponse], error)
	GetProfile(context.Context, *connect.Request[cpv1.GetProfileRequest]) (*connect.Response[cpv1.GetProfileResponse], error)
	ListProfiles(context.Context, *connect.Request[cpv1.ListProfilesRequest]) (*connect.Response[cpv1.ListProfilesResponse], error)
	UpdateProfile(context.Context, *connect.Request[cpv1.UpdateProfileRequest]) (*connect.Response[cpv1.UpdateProfileResponse], error)
	DeleteProfile(context.Context, *connect.Request[cpv1.DeleteProfileRequest]) (*connect.Response[cpv1.DeleteProfileResponse], error)
	AddProfileEntry(context.Context, *connect.Request[cpv1.AddProfileEntryRequest]) (*connect.Response[cpv1.AddProfileEntryResponse], error)
	RemoveProfileEntry(context.Context, *connect.Request[cpv1.RemoveProfileEntryRequest]) (*connect.Response[cpv1.RemoveProfileEntryResponse], error)
	AddProfileSecretRef(context.Context, *connect.Request[cpv1.AddProfileSecretRefRequest]) (*connect.Response[cpv1.AddProfileSecretRefResponse], error)
	RemoveProfileSecretRef(context.Context, *connect.Request[cpv1.RemoveProfileSecretRefRequest]) (*connect.Response[cpv1.RemoveProfileSecretRefResponse], error)
}

// Ensure the concrete generated client satisfies the interface.
var _ profileClient = (cpv1connect.SpawnServiceClient)(nil)

// ---- kind/source helpers (shared with catalog.go, same package) ----

// parseProfileEntryKind maps a lowercase string to a ProfileEntryKind proto enum.
func parseProfileEntryKind(s string) (cpv1.ProfileEntryKind, error) {
	switch strings.ToLower(s) {
	case "skill":
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL, nil
	case "mcp":
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP, nil
	case "config":
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG, nil
	case "plugin":
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN, nil
	default:
		return cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_UNSPECIFIED,
			fmt.Errorf("unknown kind %q: use skill|mcp|config|plugin", s)
	}
}

// profileEntryKindLabel returns the lowercase label for a ProfileEntryKind.
func profileEntryKindLabel(k cpv1.ProfileEntryKind) string {
	switch k {
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL:
		return "skill"
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP:
		return "mcp"
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG:
		return "config"
	case cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN:
		return "plugin"
	default:
		return "unknown"
	}
}

// profileEntrySourceLabel returns the lowercase label for a ProfileEntrySource.
func profileEntrySourceLabel(s cpv1.ProfileEntrySource) string {
	switch s {
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF:
		return "catalog"
	case cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM:
		return "custom"
	default:
		return "unknown"
	}
}

// ---- shared CP group flags / client constructor ----

// cpGroupFlags returns the standard set of flags for subcommands that talk to the CP.
func cpGroupFlags() []cli.Flag {
	return []cli.Flag{
		configDirFlag(),
		&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
		&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token"},
	}
}

// newCPClient builds an authenticated SpawnServiceClient from the standard group flags.
func newCPClient(c *cli.Command) (cpv1connect.SpawnServiceClient, error) {
	dir, err := resolveDir(c)
	if err != nil {
		return nil, err
	}
	src := buildTokenSource(dir, c.String("token"), h2cClient())
	return cpv1connect.NewSpawnServiceClient(h2cClient(), c.String("cp"),
		connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src))), nil
}

// ---- CAS version helper ----

// resolveProfileVersion fetches the current profile version unless an explicit
// non-zero version is supplied via --version. This implements the read-modify-write
// CAS pattern: the caller passes the returned value as ExpectedVersion.
func resolveProfileVersion(ctx context.Context, c profileClient, profileID string, explicit uint64) (uint64, error) {
	if explicit > 0 {
		return explicit, nil
	}
	resp, err := c.GetProfile(ctx, connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		return 0, fmt.Errorf("resolve version (GetProfile): %w", err)
	}
	return resp.Msg.GetProfile().GetVersion(), nil
}

// ---- runProfile* functions (testable: take narrow interface + io.Writer) ----

func runProfileCreate(ctx context.Context, c profileClient, out io.Writer, name string) error {
	resp, err := c.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{Name: name}))
	if err != nil {
		return fmt.Errorf("create profile: %w", err)
	}
	fmt.Fprintf(out, "created profile %s (version %d)\n", resp.Msg.GetProfileId(), resp.Msg.GetVersion())
	return nil
}

func runProfileList(ctx context.Context, c profileClient, out io.Writer) error {
	resp, err := c.ListProfiles(ctx, connect.NewRequest(&cpv1.ListProfilesRequest{}))
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE ID\tNAME\tVERSION\tUPDATED")
	for _, p := range resp.Msg.GetProfiles() {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n",
			p.GetProfileId(), p.GetName(), p.GetVersion(),
			time.Unix(p.GetUpdatedAt(), 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runProfileShow(ctx context.Context, c profileClient, out io.Writer, profileID string) error {
	resp, err := c.GetProfile(ctx, connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		return fmt.Errorf("get profile: %w", err)
	}
	p := resp.Msg.GetProfile()
	fmt.Fprintf(out, "Profile:  %s\n", p.GetProfileId())
	fmt.Fprintf(out, "Name:     %s\n", p.GetName())
	fmt.Fprintf(out, "Version:  %d\n", p.GetVersion())
	fmt.Fprintf(out, "Updated:  %s\n", time.Unix(p.GetUpdatedAt(), 0).Format(time.RFC3339))
	if len(p.GetEntries()) > 0 {
		fmt.Fprintln(out, "\nEntries:")
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "  ENTRY ID\tKIND\tNAME\tSOURCE\tCATALOG")
		for _, e := range p.GetEntries() {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
				e.GetEntryId(), profileEntryKindLabel(e.GetKind()), e.GetName(),
				profileEntrySourceLabel(e.GetSource()), e.GetCatalogId())
		}
		_ = w.Flush()
	}
	if len(p.GetSecretIds()) > 0 {
		fmt.Fprintln(out, "\nSecret refs:")
		for _, s := range p.GetSecretIds() {
			fmt.Fprintf(out, "  %s\n", s)
		}
	}
	return nil
}

func runProfileRename(ctx context.Context, c profileClient, out io.Writer, profileID, newName string, explicitVersion uint64) error {
	ver, err := resolveProfileVersion(ctx, c, profileID, explicitVersion)
	if err != nil {
		return err
	}
	resp, err := c.UpdateProfile(ctx, connect.NewRequest(&cpv1.UpdateProfileRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		Name:            newName,
	}))
	if err != nil {
		return fmt.Errorf("rename profile: %w", err)
	}
	fmt.Fprintf(out, "renamed profile %s to %q (version %d)\n", profileID, newName, resp.Msg.GetVersion())
	return nil
}

func runProfileDelete(ctx context.Context, c profileClient, out io.Writer, profileID string) error {
	_, err := c.DeleteProfile(ctx, connect.NewRequest(&cpv1.DeleteProfileRequest{ProfileId: profileID}))
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	fmt.Fprintf(out, "deleted profile %s\n", profileID)
	return nil
}

// entryAddParams holds the parsed flags for `profile entry add`.
type entryAddParams struct {
	Kind          string
	Name          string
	CatalogID     string
	CustomInline  []byte
	Targets       []string
	McpSecretRefs []string
	Version       uint64
}

func runProfileEntryAdd(ctx context.Context, c profileClient, out io.Writer, profileID string, p entryAddParams) error {
	if p.CatalogID == "" && p.CustomInline == nil {
		return fmt.Errorf("exactly one of --catalog or --custom-file is required")
	}
	if p.CatalogID != "" && p.CustomInline != nil {
		return fmt.Errorf("--catalog and --custom-file are mutually exclusive")
	}
	kind, err := parseProfileEntryKind(p.Kind)
	if err != nil {
		return err
	}
	source := cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF
	if p.CustomInline != nil {
		source = cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM
	}
	ver, err := resolveProfileVersion(ctx, c, profileID, p.Version)
	if err != nil {
		return err
	}
	entry := &cpv1.ProfileEntry{
		Kind:          kind,
		Name:          p.Name,
		Source:        source,
		CatalogId:     p.CatalogID,
		CustomInline:  p.CustomInline,
		Targets:       p.Targets,
		McpSecretRefs: p.McpSecretRefs,
	}
	resp, err := c.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		Entry:           entry,
	}))
	if err != nil {
		return fmt.Errorf("add profile entry: %w", err)
	}
	fmt.Fprintf(out, "added entry %s (profile version %d)\n", resp.Msg.GetEntryId(), resp.Msg.GetVersion())
	return nil
}

func runProfileEntryRemove(ctx context.Context, c profileClient, out io.Writer, profileID, entryID string, explicitVersion uint64) error {
	ver, err := resolveProfileVersion(ctx, c, profileID, explicitVersion)
	if err != nil {
		return err
	}
	resp, err := c.RemoveProfileEntry(ctx, connect.NewRequest(&cpv1.RemoveProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		EntryId:         entryID,
	}))
	if err != nil {
		return fmt.Errorf("remove profile entry: %w", err)
	}
	fmt.Fprintf(out, "removed entry %s (profile version %d)\n", entryID, resp.Msg.GetVersion())
	return nil
}

func runProfileSecretAdd(ctx context.Context, c profileClient, out io.Writer, profileID, secretID string, explicitVersion uint64) error {
	ver, err := resolveProfileVersion(ctx, c, profileID, explicitVersion)
	if err != nil {
		return err
	}
	resp, err := c.AddProfileSecretRef(ctx, connect.NewRequest(&cpv1.AddProfileSecretRefRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		SecretId:        secretID,
	}))
	if err != nil {
		return fmt.Errorf("add profile secret ref: %w", err)
	}
	fmt.Fprintf(out, "added secret ref %s (profile version %d)\n", secretID, resp.Msg.GetVersion())
	return nil
}

func runProfileSecretRemove(ctx context.Context, c profileClient, out io.Writer, profileID, secretID string, explicitVersion uint64) error {
	ver, err := resolveProfileVersion(ctx, c, profileID, explicitVersion)
	if err != nil {
		return err
	}
	resp, err := c.RemoveProfileSecretRef(ctx, connect.NewRequest(&cpv1.RemoveProfileSecretRefRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		SecretId:        secretID,
	}))
	if err != nil {
		return fmt.Errorf("remove profile secret ref: %w", err)
	}
	fmt.Fprintf(out, "removed secret ref %s (profile version %d)\n", secretID, resp.Msg.GetVersion())
	return nil
}

// ---- CLI wiring ----

func profileCmd() *cli.Command {
	return &cli.Command{
		Name:  "profile",
		Usage: "manage customization profiles (CRUD, entries, secret refs)",
		Commands: []*cli.Command{
			profileCreateCmd(),
			profileListCmd(),
			profileShowCmd(),
			profileRenameCmd(),
			profileDeleteCmd(),
			profileEntryCmd(),
			profileSecretCmd(),
		},
	}
}

func profileCreateCmd() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "create a new profile",
		ArgsUsage: "<name>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl profile create <name>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileCreate(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list profiles",
		Flags: cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileList(ctx, client, c.Writer); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileShowCmd() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "show a profile's entries and secret refs",
		ArgsUsage: "<profile-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl profile show <profile-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileShow(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileRenameCmd() *cli.Command {
	return &cli.Command{
		Name:      "rename",
		Usage:     "rename a profile (CAS: fetches current version unless --version given)",
		ArgsUsage: "<profile-id> <new-name>",
		Flags: append(cpGroupFlags(),
			&cli.UintFlag{Name: "version", Usage: "explicit expected version (skips GetProfile fetch)"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl profile rename <profile-id> <new-name>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileRename(ctx, client, c.Writer, c.Args().Get(0), c.Args().Get(1), uint64(c.Uint("version"))); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "delete a profile",
		ArgsUsage: "<profile-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl profile delete <profile-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileDelete(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileEntryCmd() *cli.Command {
	return &cli.Command{
		Name:  "entry",
		Usage: "manage profile entries",
		Commands: []*cli.Command{
			profileEntryAddCmd(),
			profileEntryRemoveCmd(),
		},
	}
}

func profileEntryAddCmd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "add an entry to a profile",
		ArgsUsage: "<profile-id>",
		Flags: append(cpGroupFlags(),
			&cli.StringFlag{Name: "kind", Required: true, Usage: "entry kind: skill|mcp|config|plugin"},
			&cli.StringFlag{Name: "name", Required: true, Usage: "entry name"},
			&cli.StringFlag{Name: "catalog", Usage: "catalog entry id (source=catalog-ref)"},
			&cli.StringFlag{Name: "custom-file", Usage: "path to custom content file (source=custom)"},
			&cli.StringSliceFlag{Name: "target", Usage: "target path(s) (repeatable)"},
			&cli.StringSliceFlag{Name: "mcp-secret", Usage: "MCP secret ref id(s) (repeatable)"},
			&cli.UintFlag{Name: "version", Usage: "explicit expected version (skips GetProfile fetch)"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl profile entry add <profile-id> [flags]", 2)
			}
			var customInline []byte
			if cf := c.String("custom-file"); cf != "" {
				b, err := os.ReadFile(cf)
				if err != nil {
					return cli.Exit(fmt.Sprintf("read --custom-file: %v", err), 1)
				}
				customInline = b
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			p := entryAddParams{
				Kind:          c.String("kind"),
				Name:          c.String("name"),
				CatalogID:     c.String("catalog"),
				CustomInline:  customInline,
				Targets:       c.StringSlice("target"),
				McpSecretRefs: c.StringSlice("mcp-secret"),
				Version:       uint64(c.Uint("version")),
			}
			if err := runProfileEntryAdd(ctx, client, c.Writer, c.Args().Get(0), p); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileEntryRemoveCmd() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove an entry from a profile",
		ArgsUsage: "<profile-id> <entry-id>",
		Flags: append(cpGroupFlags(),
			&cli.UintFlag{Name: "version", Usage: "explicit expected version (skips GetProfile fetch)"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl profile entry remove <profile-id> <entry-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileEntryRemove(ctx, client, c.Writer, c.Args().Get(0), c.Args().Get(1), uint64(c.Uint("version"))); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileSecretCmd() *cli.Command {
	return &cli.Command{
		Name:  "secret",
		Usage: "manage profile secret refs",
		Commands: []*cli.Command{
			profileSecretAddCmd(),
			profileSecretRemoveCmd(),
		},
	}
}

func profileSecretAddCmd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "attach a secret ref to a profile",
		ArgsUsage: "<profile-id> <secret-id>",
		Flags: append(cpGroupFlags(),
			&cli.UintFlag{Name: "version", Usage: "explicit expected version (skips GetProfile fetch)"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl profile secret add <profile-id> <secret-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileSecretAdd(ctx, client, c.Writer, c.Args().Get(0), c.Args().Get(1), uint64(c.Uint("version"))); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func profileSecretRemoveCmd() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "detach a secret ref from a profile",
		ArgsUsage: "<profile-id> <secret-id>",
		Flags: append(cpGroupFlags(),
			&cli.UintFlag{Name: "version", Usage: "explicit expected version (skips GetProfile fetch)"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl profile secret remove <profile-id> <secret-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runProfileSecretRemove(ctx, client, c.Writer, c.Args().Get(0), c.Args().Get(1), uint64(c.Uint("version"))); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}
