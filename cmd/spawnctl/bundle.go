package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// bundleClient is the subset of the cp.v1 client that the bundle commands drive.
// Narrowed to an interface so the commands are unit-testable with a fake client.
type bundleClient interface {
	ListBundles(context.Context, *connect.Request[cpv1.ListBundlesRequest]) (*connect.Response[cpv1.ListBundlesResponse], error)
	GetBundle(context.Context, *connect.Request[cpv1.GetBundleRequest]) (*connect.Response[cpv1.GetBundleResponse], error)
	ReingestBundle(context.Context, *connect.Request[cpv1.ReingestBundleRequest]) (*connect.Response[cpv1.ReingestBundleResponse], error)
}

// Ensure the concrete generated client satisfies the interface.
var _ bundleClient = (cpv1connect.SpawnServiceClient)(nil)

// ---- runBundle* functions (testable: take narrow interface + io.Writer) ----

func runBundleList(ctx context.Context, c bundleClient, out io.Writer) error {
	resp, err := c.ListBundles(ctx, connect.NewRequest(&cpv1.ListBundlesRequest{}))
	if err != nil {
		return fmt.Errorf("list bundles: %w", err)
	}
	bundles := resp.Msg.GetBundles()
	if len(bundles) == 0 {
		fmt.Fprintln(out, "no bundles")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "BUNDLE ID\tNAME\tSOURCE\tREF\tSUBDIR\tVERSION\tMEMBERS\tUPDATED")
	for _, b := range bundles {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\tv%d\t%d\t%s\n",
			b.GetBundleId(), b.GetName(), b.GetSourceUrl(), b.GetSourceRef(), b.GetSourceSubdir(),
			b.GetLatestSeq(), b.GetMemberCount(), time.Unix(b.GetUpdatedAt(), 0).Format(time.RFC3339))
	}
	return w.Flush()
}

func runBundleShow(ctx context.Context, c bundleClient, out io.Writer, bundleID string) error {
	resp, err := c.GetBundle(ctx, connect.NewRequest(&cpv1.GetBundleRequest{BundleId: bundleID}))
	if err != nil {
		return fmt.Errorf("get bundle: %w", err)
	}
	b := resp.Msg.GetBundle()
	fmt.Fprintf(out, "Bundle ID:   %s\n", b.GetBundleId())
	fmt.Fprintf(out, "Name:        %s\n", b.GetName())
	fmt.Fprintf(out, "Source:      %s\n", b.GetSourceUrl())
	fmt.Fprintf(out, "Source ref:  %s\n", b.GetSourceRef())
	fmt.Fprintf(out, "Source subdir: %s\n", b.GetSourceSubdir())
	fmt.Fprintf(out, "Created:     %s\n", time.Unix(b.GetCreatedAt(), 0).Format(time.RFC3339))
	fmt.Fprintf(out, "Updated:     %s\n", time.Unix(b.GetUpdatedAt(), 0).Format(time.RFC3339))

	fmt.Fprintln(out, "\nVersions:")
	vw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(vw, "SEQ\tVERSION ID\tCOMMIT\tCREATED")
	for _, v := range resp.Msg.GetVersions() {
		fmt.Fprintf(vw, "%d\t%s\t%s\t%s\n",
			v.GetSeq(), v.GetVersionId(), shortHash(v.GetSourceCommit()), time.Unix(v.GetCreatedAt(), 0).Format(time.RFC3339))
	}
	if err := vw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(out, "\nMembers (latest version v%d):\n", b.GetLatestSeq())
	mw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(mw, "#\tNAME\tSUBDIR\tCATALOG ID\tSHA256\tDESCRIPTION")
	for _, m := range resp.Msg.GetMembers() {
		// catalog_id and the full (untruncated) sha256 are printed, not shortened, so this row can
		// drive `catalog deny <sha>` or a per-member attach directly — no second lookup (sp-mwco.3.6).
		fmt.Fprintf(mw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			m.GetPosition(), m.GetName(), m.GetSourceSubdir(), m.GetCatalogId(), m.GetSha256(), m.GetDescription())
	}
	return mw.Flush()
}

func runBundleReingest(ctx context.Context, c bundleClient, out io.Writer, bundleID string) error {
	resp, err := c.ReingestBundle(ctx, connect.NewRequest(&cpv1.ReingestBundleRequest{BundleId: bundleID}))
	if err != nil {
		return fmt.Errorf("reingest bundle: %w", err)
	}
	r := resp.Msg
	if r.GetNotModified() {
		fmt.Fprintf(out, "up to date (version %s, upstream returned 304)\n", r.GetVersionId())
		return nil
	}
	if !r.GetChanged() {
		fmt.Fprintf(out, "no new version (already at %s)\n", r.GetVersionId())
		return nil
	}
	fmt.Fprintf(out, "new version %s (from %s)\n", r.GetVersionId(), r.GetFromVersionId())
	if len(r.GetAddedSubdirs()) > 0 {
		fmt.Fprintln(out, "Added:")
		for _, s := range r.GetAddedSubdirs() {
			fmt.Fprintf(out, "  %s\n", s)
		}
	}
	if len(r.GetUpdatedSubdirs()) > 0 {
		fmt.Fprintln(out, "Updated:")
		for _, s := range r.GetUpdatedSubdirs() {
			fmt.Fprintf(out, "  %s\n", s)
		}
	}
	if len(r.GetRemovedSubdirs()) > 0 {
		fmt.Fprintln(out, "Removed:")
		for _, s := range r.GetRemovedSubdirs() {
			fmt.Fprintf(out, "  %s\n", s)
		}
	}
	if len(r.GetWarnings()) > 0 {
		fmt.Fprintln(out, "Warnings:")
		for _, s := range r.GetWarnings() {
			fmt.Fprintf(out, "  %s\n", s)
		}
	}
	if r.GetDiffToken() != "" {
		fmt.Fprintf(out, "diff token: %s\n", r.GetDiffToken())
		fmt.Fprintln(out, "(view the diff before re-pinning a profile onto this version)")
	}
	return nil
}

// ---- CLI wiring ----

func bundleCmd() *cli.Command {
	return &cli.Command{
		Name:  "bundle",
		Usage: "inspect and refresh ingested skill bundles",
		Commands: []*cli.Command{
			bundleListCmd(),
			bundleShowCmd(),
			bundleReingestCmd(),
		},
	}
}

func bundleListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list your bundles",
		Flags: cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runBundleList(ctx, client, c.Writer); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func bundleShowCmd() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "show a bundle's provenance, versions, and latest members",
		ArgsUsage: "<bundle-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl bundle show <bundle-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runBundleShow(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func bundleReingestCmd() *cli.Command {
	return &cli.Command{
		Name:      "reingest",
		Usage:     "re-fetch a bundle's upstream source and cut a new version if it changed",
		ArgsUsage: "<bundle-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl bundle reingest <bundle-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runBundleReingest(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}
