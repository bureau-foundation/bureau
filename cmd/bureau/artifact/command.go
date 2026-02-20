// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package artifact implements the "bureau artifact" CLI subcommands
// for interacting with the artifact service over its Unix socket.
//
// Connection parameters default to the in-sandbox paths where the
// daemon provisions sockets and tokens. Operators running outside a
// sandbox can override with --socket and --token-file flags (or the
// BUREAU_ARTIFACT_SOCKET and BUREAU_ARTIFACT_TOKEN environment
// variables).
package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/artifact"
)

// Sandbox-standard paths for the artifact service role. When an agent
// declares required_services: ["artifact"], the daemon bind-mounts the
// artifact service socket and writes a service token in the token
// subdirectory.
const (
	sandboxSocketPath = "/run/bureau/service/artifact.sock"
	sandboxTokenPath  = "/run/bureau/service/token/artifact.token"
)

// Command returns the top-level "artifact" command with all subcommands.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "artifact",
		Summary: "Manage Bureau artifacts (store, fetch, list, tag)",
		Description: `Interact with the Bureau artifact service.

Store, fetch, list, and manage content-addressed artifacts. Artifacts
are identified by BLAKE3 hashes and can be referenced by short refs
(art-<hex>) or mutable tags (name→hash pointers).

Connection defaults to the in-sandbox artifact service socket. Override
with --socket/--token-file flags or BUREAU_ARTIFACT_SOCKET and
BUREAU_ARTIFACT_TOKEN environment variables.`,
		Subcommands: []*cli.Command{
			storeCommand(),
			fetchCommand(),
			showCommand(),
			existsCommand(),
			listCommand(),
			tagCommand(),
			resolveCommand(),
			tagsCommand(),
			deleteTagCommand(),
			pinCommand(),
			unpinCommand(),
			gcCommand(),
			statusCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Store a file and get its ref",
				Command:     "bureau artifact store model.bin --content-type application/octet-stream",
			},
			{
				Description: "Fetch an artifact to a file",
				Command:     "bureau artifact fetch art-a3f9b2c1e7d4 -o model.bin",
			},
			{
				Description: "List all text artifacts",
				Command:     "bureau artifact list --content-type text/plain",
			},
			{
				Description: "Tag an artifact for easy reference",
				Command:     "bureau artifact tag model/latest art-a3f9b2c1e7d4",
			},
			{
				Description: "Fetch by tag name",
				Command:     "bureau artifact fetch model/latest -o latest-model.bin",
			},
		},
	}
}

// ArtifactConnection manages socket and token flags for artifact commands.
// Implements [cli.FlagBinder] so it integrates with the params struct system
// while handling dynamic defaults from environment variables. Excluded from
// JSON Schema generation since MCP callers don't specify socket paths.
//
// Exported so that embedded struct fields are visible to reflection in
// [cli.FlagsFromParams] — unexported embedded types cause field.IsExported()
// to return false, silently skipping FlagBinder detection.
type ArtifactConnection struct {
	SocketPath string
	TokenPath  string
}

// AddFlags registers --socket and --token-file flags with dynamic defaults
// from BUREAU_ARTIFACT_SOCKET and BUREAU_ARTIFACT_TOKEN environment variables.
// If neither environment variable is set, defaults are detected from the
// runtime environment: inside a sandbox, the RequiredServices mount points
// are used; outside, the host-side principal socket path.
func (c *ArtifactConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultArtifactSocketPath()
	if envSocket := os.Getenv("BUREAU_ARTIFACT_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultArtifactTokenPath()
	if envToken := os.Getenv("BUREAU_ARTIFACT_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "artifact service socket path")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file")
}

// defaultArtifactSocketPath returns the default artifact service socket path.
// Inside a sandbox, the daemon bind-mounts the artifact service socket at
// the sandboxSocketPath. Outside a sandbox, the same path is used as the
// default — operators running the CLI directly must specify --socket or
// set BUREAU_ARTIFACT_SOCKET (the actual host-side socket is fleet-scoped
// and not discoverable without fleet context).
func defaultArtifactSocketPath() string {
	return sandboxSocketPath
}

// defaultArtifactTokenPath returns the default artifact service token path.
// The daemon-provisioned token is at sandboxTokenPath. Operators outside
// a sandbox must specify --token-file or set BUREAU_ARTIFACT_TOKEN.
func defaultArtifactTokenPath() string {
	return sandboxTokenPath
}

// connect creates an artifact client from the connection parameters.
func (c *ArtifactConnection) connect() (*artifact.Client, error) {
	return artifact.NewClient(c.SocketPath, c.TokenPath)
}

// formatSize returns a human-readable file size.
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// --- store ---

type storeParams struct {
	ArtifactConnection
	cli.JSONOutput
	ContentType string   `json:"content_type"  flag:"content-type" desc:"MIME content type (guessed from filename if omitted)"`
	Description string   `json:"description"   flag:"description"  desc:"human-readable description"`
	Tag         string   `json:"tag"           flag:"tag"          desc:"tag the artifact after storing"`
	CachePolicy string   `json:"cache_policy"  flag:"cache-policy" desc:"cache policy (e.g. pin, replicate)"`
	Visibility  string   `json:"visibility"    flag:"visibility"   desc:"visibility level: private (default, encrypted for external transfer) or public"`
	TTL         string   `json:"ttl"           flag:"ttl"          desc:"time-to-live (e.g. 72h, 7d)"`
	Labels      []string `json:"labels"        flag:"label"        desc:"labels (repeatable)"`
	PushTo      []string `json:"push_to"       flag:"push-to"      desc:"push artifact to machine(s) after storing (repeatable)"`
}

func storeCommand() *cli.Command {
	var params storeParams

	return &cli.Command{
		Name:    "store",
		Summary: "Store an artifact from a file or stdin",
		Usage:   "bureau artifact store [file] [flags]",
		Description: `Upload content to the artifact store.

Reads from the named file, or from stdin if no file is given (or file
is "-"). The artifact ref is printed to stdout on success.

Content type is guessed from the filename extension when --content-type
is not set. Falls back to "application/octet-stream" for stdin or
unrecognized extensions.`,
		Examples: []cli.Example{
			{
				Description: "Store a file",
				Command:     "bureau artifact store model.bin",
			},
			{
				Description: "Store from stdin with explicit content type",
				Command:     "cat data.csv | bureau artifact store --content-type text/csv",
			},
			{
				Description: "Store with a tag and labels",
				Command:     "bureau artifact store weights.pt --tag model/latest --label training --label v2",
			},
			{
				Description: "Store and push to a remote machine",
				Command:     "bureau artifact store model.bin --push-to machine/gpu-server-1",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.StoreResponse{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/artifact/store"},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			var content io.Reader
			var filename string
			var size int64

			if len(args) == 0 || args[0] == "-" {
				content = os.Stdin
				size = artifact.SizeUnknown
			} else {
				filePath := args[0]
				file, err := os.Open(filePath)
				if err != nil {
					return cli.Internal("opening %s: %w", filePath, err)
				}
				defer file.Close()

				info, err := file.Stat()
				if err != nil {
					return cli.Internal("stat %s: %w", filePath, err)
				}
				size = info.Size()
				content = file
				filename = file.Name()

				if params.ContentType == "" {
					params.ContentType = guessContentType(filename)
				}
			}

			if params.ContentType == "" {
				params.ContentType = "application/octet-stream"
			}

			header := &artifact.StoreHeader{
				ContentType: params.ContentType,
				Filename:    filename,
				Size:        size,
				Description: params.Description,
				Labels:      params.Labels,
				Tag:         params.Tag,
				CachePolicy: params.CachePolicy,
				Visibility:  params.Visibility,
				TTL:         params.TTL,
				PushTargets: params.PushTo,
			}

			// For small files, embed data in the header.
			if size >= 0 && size <= artifact.SmallArtifactThreshold {
				data, err := io.ReadAll(content)
				if err != nil {
					return cli.Internal("reading content: %w", err)
				}
				header.Data = data
				header.Size = int64(len(data))
				content = nil
			}

			response, err := client.Store(ctx, header, content)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			// Print push results to stderr so stdout remains
			// just the ref (for composability with pipelines).
			for _, result := range response.PushResults {
				if result.OK {
					fmt.Fprintf(os.Stderr, "pushed to %s\n", result.Target)
				} else {
					fmt.Fprintf(os.Stderr, "push to %s failed: %s\n", result.Target, result.Error)
				}
			}

			fmt.Println(response.Ref)
			return nil
		},
	}
}

// --- fetch ---

type fetchParams struct {
	ArtifactConnection
	OutputPath string `json:"-" flag:"output,o" desc:"output file path (default: stdout)"`
}

func fetchCommand() *cli.Command {
	var params fetchParams

	return &cli.Command{
		Name:    "fetch",
		Summary: "Download an artifact to a file or stdout",
		Usage:   "bureau artifact fetch <ref> [flags]",
		Description: `Download artifact content by ref, short ref, or tag name.

Writes to the named output file, or to stdout if -o is not set.
The ref can be a full hash, short ref (art-<hex>), or tag name.`,
		Examples: []cli.Example{
			{
				Description: "Fetch to a file",
				Command:     "bureau artifact fetch art-a3f9b2c1e7d4 -o model.bin",
			},
			{
				Description: "Fetch by tag name to stdout",
				Command:     "bureau artifact fetch model/latest > model.bin",
			},
		},
		Params:         func() any { return &params },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/fetch"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("ref argument required\n\nUsage: bureau artifact fetch <ref> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			result, err := client.Fetch(ctx, args[0])
			if err != nil {
				return err
			}
			defer result.Content.Close()

			var output io.Writer
			if params.OutputPath != "" {
				file, err := os.Create(params.OutputPath)
				if err != nil {
					return cli.Internal("creating output file: %w", err)
				}
				defer file.Close()
				output = file
			} else {
				output = os.Stdout
			}

			if _, err := io.Copy(output, result.Content); err != nil {
				return cli.Internal("writing content: %w", err)
			}
			return nil
		},
	}
}

// --- show ---

type showParams struct {
	ArtifactConnection
	cli.JSONOutput
}

func showCommand() *cli.Command {
	var params showParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show artifact metadata",
		Usage:   "bureau artifact show <ref> [flags]",
		Description: `Display metadata for an artifact without downloading its content.

Shows content type, size, labels, description, cache policy, and
storage details.`,
		Examples: []cli.Example{
			{
				Description: "Show metadata for an artifact",
				Command:     "bureau artifact show art-a3f9b2c1e7d4",
			},
			{
				Description: "Show metadata as JSON",
				Command:     "bureau artifact show model/latest --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.ArtifactMetadata{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/show"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("ref argument required\n\nUsage: bureau artifact show <ref> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			meta, err := client.Show(ctx, args[0])
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(meta); done {
				return err
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "Ref:\t%s\n", meta.Ref)
			fmt.Fprintf(writer, "Hash:\t%x\n", meta.FileHash)
			fmt.Fprintf(writer, "Content-Type:\t%s\n", meta.ContentType)
			fmt.Fprintf(writer, "Size:\t%s (%d bytes)\n", formatSize(meta.Size), meta.Size)
			if meta.Filename != "" {
				fmt.Fprintf(writer, "Filename:\t%s\n", meta.Filename)
			}
			if meta.Description != "" {
				fmt.Fprintf(writer, "Description:\t%s\n", meta.Description)
			}
			if len(meta.Labels) > 0 {
				fmt.Fprintf(writer, "Labels:\t%s\n", strings.Join(meta.Labels, ", "))
			}
			if meta.CachePolicy != "" {
				fmt.Fprintf(writer, "Cache Policy:\t%s\n", meta.CachePolicy)
			}
			fmt.Fprintf(writer, "Visibility:\t%s\n", artifact.NormalizeVisibility(meta.Visibility))
			if meta.TTL != "" {
				fmt.Fprintf(writer, "TTL:\t%s\n", meta.TTL)
			}
			fmt.Fprintf(writer, "Compression:\t%s\n", meta.Compression)
			fmt.Fprintf(writer, "Chunks:\t%d\n", meta.ChunkCount)
			fmt.Fprintf(writer, "Containers:\t%d\n", meta.ContainerCount)
			fmt.Fprintf(writer, "Stored:\t%s\n", meta.StoredAt.Format("2006-01-02 15:04:05 UTC"))
			writer.Flush()
			return nil
		},
	}
}

// --- exists ---

type existsParams struct {
	ArtifactConnection
	cli.JSONOutput
}

func existsCommand() *cli.Command {
	var params existsParams

	return &cli.Command{
		Name:    "exists",
		Summary: "Check whether an artifact exists",
		Usage:   "bureau artifact exists <ref> [flags]",
		Description: `Check if an artifact exists in the store. Exits 0 if it exists,
1 if it does not. With --json, outputs the exists response.`,
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.ExistsResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/exists"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("ref argument required\n\nUsage: bureau artifact exists <ref> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.Exists(ctx, args[0])
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			if response.Exists {
				fmt.Printf("%s %s\n", response.Ref, formatSize(response.Size))
				return nil
			}

			fmt.Fprintf(os.Stderr, "not found: %s\n", args[0])
			return &cli.ExitError{Code: 1}
		},
	}
}

// --- list ---

type listParams struct {
	ArtifactConnection
	cli.JSONOutput
	ContentType string `json:"content_type" flag:"content-type" desc:"filter by content type"`
	Label       string `json:"label"        flag:"label"        desc:"filter by label"`
	CachePolicy string `json:"cache_policy" flag:"cache-policy" desc:"filter by cache policy"`
	Visibility  string `json:"visibility"   flag:"visibility"   desc:"filter by visibility"`
	MinSize     int64  `json:"min_size"     flag:"min-size"     desc:"minimum size in bytes"`
	MaxSize     int64  `json:"max_size"     flag:"max-size"     desc:"maximum size in bytes"`
	Limit       int    `json:"limit"        flag:"limit"        desc:"maximum results (default: server decides)"`
	Offset      int    `json:"offset"       flag:"offset"       desc:"skip this many results"`
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List artifacts with optional filters",
		Usage:   "bureau artifact list [flags]",
		Description: `Query the artifact index with optional filters. Filters are AND-combined:
only artifacts matching all specified criteria are returned. Results
are sorted by storage time (newest first).`,
		Examples: []cli.Example{
			{
				Description: "List all artifacts",
				Command:     "bureau artifact list",
			},
			{
				Description: "List text artifacts with a label",
				Command:     "bureau artifact list --content-type text/plain --label docs",
			},
			{
				Description: "List large artifacts (>1MB)",
				Command:     "bureau artifact list --min-size 1048576",
			},
			{
				Description: "Paginate: second page of 10",
				Command:     "bureau artifact list --limit 10 --offset 10",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.ListResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/list"},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.List(ctx, artifact.ListRequest{
				ContentType: params.ContentType,
				Label:       params.Label,
				CachePolicy: params.CachePolicy,
				Visibility:  params.Visibility,
				MinSize:     params.MinSize,
				MaxSize:     params.MaxSize,
				Limit:       params.Limit,
				Offset:      params.Offset,
			})
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			if len(response.Artifacts) == 0 {
				fmt.Println("No artifacts found.")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "REF\tSIZE\tTYPE\tSTORED\n")
			for _, entry := range response.Artifacts {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
					entry.Ref,
					formatSize(entry.Size),
					entry.ContentType,
					entry.StoredAt,
				)
			}
			writer.Flush()

			if response.Total > len(response.Artifacts) {
				fmt.Fprintf(os.Stderr, "\nShowing %d of %d artifacts.\n",
					len(response.Artifacts), response.Total)
			}
			return nil
		},
	}
}

// --- tag ---

type tagParams struct {
	ArtifactConnection
	cli.JSONOutput
	Optimistic       bool   `json:"optimistic" flag:"optimistic" desc:"overwrite existing tag without CAS check"`
	ExpectedPrevious string `json:"expected"   flag:"expected"   desc:"expected current target hash (for CAS update)"`
}

func tagCommand() *cli.Command {
	var params tagParams

	return &cli.Command{
		Name:    "tag",
		Summary: "Create or update a mutable tag",
		Usage:   "bureau artifact tag <name> <ref> [flags]",
		Description: `Create or update a tag pointing to an artifact. Tags are mutable
name→hash pointers that provide stable references to artifacts.

By default, creating a tag that already exists fails (compare-and-swap
semantics). Use --optimistic for last-writer-wins, or --expected to
specify the previous target hash for CAS updates.`,
		Examples: []cli.Example{
			{
				Description: "Create a new tag",
				Command:     "bureau artifact tag model/latest art-a3f9b2c1e7d4",
			},
			{
				Description: "Overwrite a tag unconditionally",
				Command:     "bureau artifact tag model/latest art-b5e8d3f1a2c0 --optimistic",
			},
			{
				Description: "CAS update (only if current target matches expected)",
				Command:     "bureau artifact tag model/latest art-new --expected art-old-hash",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.TagResponse{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/artifact/tag"},
		Run: func(args []string) error {
			if len(args) < 2 {
				return cli.Validation("name and ref arguments required\n\nUsage: bureau artifact tag <name> <ref> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.Tag(ctx, args[0], args[1], params.Optimistic, params.ExpectedPrevious)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			fmt.Printf("%s → %s\n", response.Name, response.Ref)
			return nil
		},
	}
}

// --- resolve ---

type resolveParams struct {
	ArtifactConnection
	cli.JSONOutput
}

func resolveCommand() *cli.Command {
	var params resolveParams

	return &cli.Command{
		Name:    "resolve",
		Summary: "Resolve a ref or tag to a full hash",
		Usage:   "bureau artifact resolve <ref> [flags]",
		Description: `Resolve a short ref (art-<hex>), tag name, or full hash to the
canonical artifact reference. Useful for scripting: the output is
always the full ref.`,
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.ResolveResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/resolve"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("ref argument required\n\nUsage: bureau artifact resolve <ref> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.Resolve(ctx, args[0])
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			fmt.Println(response.Ref)
			return nil
		},
	}
}

// --- tags ---

type tagsParams struct {
	ArtifactConnection
	cli.JSONOutput
	Prefix string `json:"prefix" flag:"prefix" desc:"filter tags by name prefix"`
}

func tagsCommand() *cli.Command {
	var params tagsParams

	return &cli.Command{
		Name:        "tags",
		Summary:     "List tags",
		Usage:       "bureau artifact tags [flags]",
		Description: `List all tags, optionally filtered by prefix.`,
		Examples: []cli.Example{
			{
				Description: "List all tags",
				Command:     "bureau artifact tags",
			},
			{
				Description: "List model tags",
				Command:     "bureau artifact tags --prefix model/",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.TagsResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/tags"},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.Tags(ctx, params.Prefix)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			if len(response.Tags) == 0 {
				fmt.Println("No tags found.")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "NAME\tREF\n")
			for _, tag := range response.Tags {
				fmt.Fprintf(writer, "%s\t%s\n", tag.Name, tag.Ref)
			}
			writer.Flush()
			return nil
		},
	}
}

// --- delete-tag ---

type deleteTagParams struct {
	ArtifactConnection
}

func deleteTagCommand() *cli.Command {
	var params deleteTagParams

	return &cli.Command{
		Name:           "delete-tag",
		Summary:        "Delete a tag",
		Usage:          "bureau artifact delete-tag <name> [flags]",
		Params:         func() any { return &params },
		Annotations:    cli.Destructive(),
		RequiredGrants: []string{"command/artifact/delete-tag"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("tag name required\n\nUsage: bureau artifact delete-tag <name> [flags]")
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.DeleteTag(ctx, args[0])
			if err != nil {
				return err
			}

			fmt.Printf("deleted: %s\n", response.Deleted)
			return nil
		},
	}
}

// --- pin / unpin ---

type pinParams struct {
	ArtifactConnection
	cli.JSONOutput
}

// pinToggleCommand builds either "pin" or "unpin" — the two commands
// differ only in name, help text, which client method is called, and
// the human-readable confirmation verb.
func pinToggleCommand(
	name, summary, verb string,
	annotations *cli.ToolAnnotations,
	call func(*artifact.Client, context.Context, string) (*artifact.PinResponse, error),
) *cli.Command {
	var params pinParams

	usage := fmt.Sprintf("bureau artifact %s <ref> [flags]", name)

	return &cli.Command{
		Name:           name,
		Summary:        summary,
		Usage:          usage,
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.PinResponse{} },
		Annotations:    annotations,
		RequiredGrants: []string{"command/artifact/" + name},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("ref argument required\n\nUsage: %s", usage)
			}

			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := call(client, ctx, args[0])
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			fmt.Printf("%s: %s\n", verb, response.Ref)
			return nil
		},
	}
}

func pinCommand() *cli.Command {
	return pinToggleCommand("pin", "Pin an artifact (protect from GC)", "pinned",
		cli.Idempotent(), (*artifact.Client).Pin)
}

func unpinCommand() *cli.Command {
	return pinToggleCommand("unpin", "Unpin an artifact (allow GC)", "unpinned",
		cli.Idempotent(), (*artifact.Client).Unpin)
}

// --- gc ---

type gcParams struct {
	ArtifactConnection
	cli.JSONOutput
	DryRun bool `json:"dry_run" flag:"dry-run" desc:"report what would be collected without deleting"`
}

func gcCommand() *cli.Command {
	var params gcParams

	return &cli.Command{
		Name:    "gc",
		Summary: "Run garbage collection",
		Usage:   "bureau artifact gc [flags]",
		Description: `Run mark-and-sweep garbage collection on the artifact store. Removes
artifacts with expired TTLs that are not protected by pins or tags.

Use --dry-run to see what would be removed without actually deleting.`,
		Examples: []cli.Example{
			{
				Description: "Preview what GC would remove",
				Command:     "bureau artifact gc --dry-run",
			},
			{
				Description: "Run GC",
				Command:     "bureau artifact gc",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.GCResponse{} },
		Annotations:    cli.Destructive(),
		RequiredGrants: []string{"command/artifact/gc"},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := params.connect()
			if err != nil {
				return err
			}

			response, err := client.GC(ctx, params.DryRun)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			prefix := ""
			if response.DryRun {
				prefix = "(dry run) "
			}
			fmt.Printf("%sremoved %d artifacts, %d containers, freed %s\n",
				prefix,
				response.ArtifactsRemoved,
				response.ContainersRemoved,
				formatSize(response.BytesFreed),
			)
			return nil
		},
	}
}

// --- status ---

type statusParams struct {
	ArtifactConnection
	cli.JSONOutput
}

func statusCommand() *cli.Command {
	var params statusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show artifact service status",
		Usage:   "bureau artifact status [flags]",
		Description: `Show service liveness information. This action does not require
authentication — it is a health check.`,
		Params:         func() any { return &params },
		Output:         func() any { return &artifact.StatusResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/artifact/status"},
		Run: func(args []string) error {
			ctx := context.Background()
			// Status is unauthenticated — use token if available,
			// but don't fail if the token file is missing.
			client, err := params.connect()
			if err != nil {
				client = artifact.NewClientFromToken(params.SocketPath, nil)
			}

			response, err := client.Status(ctx)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "Uptime:\t%.0fs\n", response.UptimeSeconds)
			fmt.Fprintf(writer, "Artifacts:\t%d\n", response.Artifacts)
			fmt.Fprintf(writer, "Rooms:\t%d\n", response.Rooms)
			writer.Flush()
			return nil
		},
	}
}

// --- Content type guessing ---

// guessContentType returns a MIME type based on the file extension.
// Returns empty string if the extension is not recognized.
func guessContentType(filename string) string {
	extension := strings.ToLower(filename)
	if idx := strings.LastIndex(extension, "."); idx >= 0 {
		extension = extension[idx:]
	} else {
		return ""
	}

	types := map[string]string{
		".txt":  "text/plain",
		".csv":  "text/csv",
		".json": "application/json",
		".xml":  "application/xml",
		".html": "text/html",
		".md":   "text/markdown",
		".yaml": "application/yaml",
		".yml":  "application/yaml",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".svg":  "image/svg+xml",
		".pdf":  "application/pdf",
		".zip":  "application/zip",
		".gz":   "application/gzip",
		".tar":  "application/x-tar",
		".bin":  "application/octet-stream",
		".pt":   "application/octet-stream",
		".pth":  "application/octet-stream",
		".onnx": "application/octet-stream",
		".log":  "text/plain",
		".py":   "text/x-python",
		".go":   "text/x-go",
		".rs":   "text/x-rust",
		".c":    "text/x-c",
		".h":    "text/x-c",
		".cpp":  "text/x-c++",
	}

	if ct, ok := types[extension]; ok {
		return ct
	}
	return ""
}
