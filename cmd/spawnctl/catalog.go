package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
)

// catalogClient is the subset of the cp.v1 client that the catalog commands drive.
// Narrowed to an interface so the commands are unit-testable with a fake client.
type catalogClient interface {
	CreateCatalogEntry(context.Context, *connect.Request[cpv1.CreateCatalogEntryRequest]) (*connect.Response[cpv1.CreateCatalogEntryResponse], error)
	GetCatalogEntry(context.Context, *connect.Request[cpv1.GetCatalogEntryRequest]) (*connect.Response[cpv1.GetCatalogEntryResponse], error)
	ListCatalogEntries(context.Context, *connect.Request[cpv1.ListCatalogEntriesRequest]) (*connect.Response[cpv1.ListCatalogEntriesResponse], error)
	UpdateCatalogEntry(context.Context, *connect.Request[cpv1.UpdateCatalogEntryRequest]) (*connect.Response[cpv1.UpdateCatalogEntryResponse], error)
	DeleteCatalogEntry(context.Context, *connect.Request[cpv1.DeleteCatalogEntryRequest]) (*connect.Response[cpv1.DeleteCatalogEntryResponse], error)
	SetCatalogListing(context.Context, *connect.Request[cpv1.SetCatalogListingRequest]) (*connect.Response[cpv1.SetCatalogListingResponse], error)
}

// Ensure the concrete generated client satisfies the interface.
var _ catalogClient = (cpv1connect.SpawnServiceClient)(nil)

// ---- params ----

// catalogCreateParams holds the parsed flags for `catalog create`.
type catalogCreateParams struct {
	Kind        string
	Name        string
	Description string
	Content     []byte
}

// catalogUpdateParams holds the parsed flags for `catalog update`.
type catalogUpdateParams struct {
	Name        string
	Description string
	Content     []byte
}

// ---- runCatalog* functions (testable: take narrow interface + io.Writer) ----

func runCatalogCreate(ctx context.Context, c catalogClient, out io.Writer, p catalogCreateParams) error {
	kind, err := parseProfileEntryKind(p.Kind)
	if err != nil {
		return err
	}
	resp, err := c.CreateCatalogEntry(ctx, connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:        kind,
		Name:        p.Name,
		Description: p.Description,
		Content:     p.Content,
	}))
	if err != nil {
		return fmt.Errorf("create catalog entry: %w", err)
	}
	fmt.Fprintf(out, "created catalog entry %s\n", resp.Msg.GetCatalogId())
	return nil
}

func runCatalogList(ctx context.Context, c catalogClient, out io.Writer) error {
	resp, err := c.ListCatalogEntries(ctx, connect.NewRequest(&cpv1.ListCatalogEntriesRequest{}))
	if err != nil {
		return fmt.Errorf("list catalog entries: %w", err)
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "CATALOG ID\tKIND\tNAME\tDESCRIPTION")
	for _, e := range resp.Msg.GetEntries() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			e.GetCatalogId(), profileEntryKindLabel(e.GetKind()), e.GetName(), e.GetDescription())
	}
	return w.Flush()
}

func runCatalogShow(ctx context.Context, c catalogClient, out io.Writer, catalogID string) error {
	resp, err := c.GetCatalogEntry(ctx, connect.NewRequest(&cpv1.GetCatalogEntryRequest{CatalogId: catalogID}))
	if err != nil {
		return fmt.Errorf("get catalog entry: %w", err)
	}
	e := resp.Msg.GetEntry()
	fmt.Fprintf(out, "Catalog ID:  %s\n", e.GetCatalogId())
	fmt.Fprintf(out, "Creator:     %s\n", e.GetCreatorId())
	fmt.Fprintf(out, "Kind:        %s\n", profileEntryKindLabel(e.GetKind()))
	fmt.Fprintf(out, "Name:        %s\n", e.GetName())
	fmt.Fprintf(out, "Description: %s\n", e.GetDescription())
	fmt.Fprintf(out, "Listed:      %v\n", e.GetListed())
	fmt.Fprintf(out, "Created:     %s\n", time.Unix(e.GetCreatedAt(), 0).Format(time.RFC3339))
	fmt.Fprintf(out, "Updated:     %s\n", time.Unix(e.GetUpdatedAt(), 0).Format(time.RFC3339))
	if len(e.GetContent()) > 0 {
		fmt.Fprintln(out, "\nContent:")
		_, _ = out.Write(e.GetContent())
		fmt.Fprintln(out)
	}
	return nil
}

func runCatalogUpdate(ctx context.Context, c catalogClient, out io.Writer, catalogID string, p catalogUpdateParams) error {
	_, err := c.UpdateCatalogEntry(ctx, connect.NewRequest(&cpv1.UpdateCatalogEntryRequest{
		CatalogId:   catalogID,
		Name:        p.Name,
		Description: p.Description,
		Content:     p.Content,
	}))
	if err != nil {
		return fmt.Errorf("update catalog entry: %w", err)
	}
	fmt.Fprintf(out, "updated catalog entry %s\n", catalogID)
	return nil
}

func runCatalogDelete(ctx context.Context, c catalogClient, out io.Writer, catalogID string) error {
	_, err := c.DeleteCatalogEntry(ctx, connect.NewRequest(&cpv1.DeleteCatalogEntryRequest{CatalogId: catalogID}))
	if err != nil {
		return fmt.Errorf("delete catalog entry: %w", err)
	}
	fmt.Fprintf(out, "deleted catalog entry %s\n", catalogID)
	return nil
}

func runCatalogSetListing(ctx context.Context, c catalogClient, out io.Writer, catalogID string, listed bool) error {
	_, err := c.SetCatalogListing(ctx, connect.NewRequest(&cpv1.SetCatalogListingRequest{
		CatalogId: catalogID,
		Listed:    listed,
	}))
	if err != nil {
		return fmt.Errorf("set catalog listing: %w", err)
	}
	fmt.Fprintf(out, "set listing=%v for catalog entry %s\n", listed, catalogID)
	return nil
}

// ---- CLI wiring ----

func catalogCmd() *cli.Command {
	return &cli.Command{
		Name:  "catalog",
		Usage: "manage curated catalog entries (CRUD, listing)",
		Commands: []*cli.Command{
			catalogCreateCmd(),
			catalogListCmd(),
			catalogShowCmd(),
			catalogUpdateCmd(),
			catalogDeleteCmd(),
			catalogSetListingCmd(),
		},
	}
}

func catalogCreateCmd() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "create a new catalog entry",
		ArgsUsage: "<name>",
		Flags: append(cpGroupFlags(),
			&cli.StringFlag{Name: "kind", Required: true, Usage: "entry kind: skill|mcp|config|plugin"},
			&cli.StringFlag{Name: "description", Usage: "entry description"},
			&cli.StringFlag{Name: "content-file", Usage: "path to content file"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl catalog create <name> [flags]", 2)
			}
			var content []byte
			if cf := c.String("content-file"); cf != "" {
				b, err := os.ReadFile(cf)
				if err != nil {
					return cli.Exit(fmt.Sprintf("read --content-file: %v", err), 1)
				}
				content = b
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			p := catalogCreateParams{
				Kind:        c.String("kind"),
				Name:        c.Args().Get(0),
				Description: c.String("description"),
				Content:     content,
			}
			if err := runCatalogCreate(ctx, client, c.Writer, p); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func catalogListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list catalog entries",
		Flags: cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runCatalogList(ctx, client, c.Writer); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func catalogShowCmd() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "show a catalog entry's details and content",
		ArgsUsage: "<catalog-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl catalog show <catalog-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runCatalogShow(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func catalogUpdateCmd() *cli.Command {
	return &cli.Command{
		Name:      "update",
		Usage:     "update a catalog entry's name, description, or content",
		ArgsUsage: "<catalog-id>",
		Flags: append(cpGroupFlags(),
			&cli.StringFlag{Name: "name", Usage: "new name"},
			&cli.StringFlag{Name: "description", Usage: "new description"},
			&cli.StringFlag{Name: "content-file", Usage: "path to new content file"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl catalog update <catalog-id> [flags]", 2)
			}
			var content []byte
			if cf := c.String("content-file"); cf != "" {
				b, err := os.ReadFile(cf)
				if err != nil {
					return cli.Exit(fmt.Sprintf("read --content-file: %v", err), 1)
				}
				content = b
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			p := catalogUpdateParams{
				Name:        c.String("name"),
				Description: c.String("description"),
				Content:     content,
			}
			if err := runCatalogUpdate(ctx, client, c.Writer, c.Args().Get(0), p); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func catalogDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "delete a catalog entry",
		ArgsUsage: "<catalog-id>",
		Flags:     cpGroupFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl catalog delete <catalog-id>", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runCatalogDelete(ctx, client, c.Writer, c.Args().Get(0)); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

func catalogSetListingCmd() *cli.Command {
	return &cli.Command{
		Name:      "set-listing",
		Usage:     "set the public listing flag for a catalog entry",
		ArgsUsage: "<catalog-id>",
		Flags: append(cpGroupFlags(),
			&cli.BoolFlag{Name: "listed", Usage: "whether the entry should be publicly listed"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return cli.Exit("usage: spawnctl catalog set-listing <catalog-id> [--listed]", 2)
			}
			client, err := newCPClient(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := runCatalogSetListing(ctx, client, c.Writer, c.Args().Get(0), c.Bool("listed")); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}
