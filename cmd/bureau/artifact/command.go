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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/artifact"
)

// Default paths inside a Bureau sandbox.
const (
	defaultSocketPath = "/run/bureau/principal/service/artifact/main.sock"
	defaultTokenPath  = "/run/bureau/tokens/artifact"
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

// connectFlags adds --socket and --token-file flags to a flag set,
// returning pointers to the captured values. The flags default to
// environment variables if set, falling back to the in-sandbox paths.
func connectFlags(flagSet *pflag.FlagSet) (socketPath, tokenPath *string) {
	socketDefault := defaultSocketPath
	if envSocket := os.Getenv("BUREAU_ARTIFACT_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultTokenPath
	if envToken := os.Getenv("BUREAU_ARTIFACT_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	socket := flagSet.String("socket", socketDefault, "artifact service socket path")
	token := flagSet.String("token-file", tokenDefault, "path to service token file")
	return socket, token
}

// connect creates an artifact client from the flag values.
func connect(socketPath, tokenPath string) (*artifact.Client, error) {
	return artifact.NewClient(socketPath, tokenPath)
}

// writeJSON marshals v to stdout as indented JSON.
func writeJSON(v any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
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

func storeCommand() *cli.Command {
	var socketPath, tokenPath *string
	var contentType, description, tag, cachePolicy, visibility, ttl string
	var labels []string
	var jsonOutput bool

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
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("store", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.StringVar(&contentType, "content-type", "", "MIME content type (guessed from filename if omitted)")
			flagSet.StringVar(&description, "description", "", "human-readable description")
			flagSet.StringVar(&tag, "tag", "", "tag the artifact after storing")
			flagSet.StringVar(&cachePolicy, "cache-policy", "", "cache policy (e.g., \"pin\")")
			flagSet.StringVar(&visibility, "visibility", "", "visibility level")
			flagSet.StringVar(&ttl, "ttl", "", "time-to-live (e.g., \"72h\", \"7d\")")
			flagSet.StringSliceVar(&labels, "label", nil, "labels (repeatable)")
			flagSet.BoolVar(&jsonOutput, "json", false, "output result as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			var content io.Reader
			var filename string
			var size int64

			if len(args) == 0 || args[0] == "-" {
				// Read from stdin — size unknown, use chunked transfer.
				content = os.Stdin
				size = artifact.SizeUnknown
			} else {
				filePath := args[0]
				file, err := os.Open(filePath)
				if err != nil {
					return fmt.Errorf("opening %s: %w", filePath, err)
				}
				defer file.Close()

				info, err := file.Stat()
				if err != nil {
					return fmt.Errorf("stat %s: %w", filePath, err)
				}
				size = info.Size()
				content = file
				filename = file.Name()

				// Guess content type from extension if not specified.
				if contentType == "" {
					contentType = guessContentType(filename)
				}
			}

			if contentType == "" {
				contentType = "application/octet-stream"
			}

			header := &artifact.StoreHeader{
				ContentType: contentType,
				Filename:    filename,
				Size:        size,
				Description: description,
				Labels:      labels,
				Tag:         tag,
				CachePolicy: cachePolicy,
				Visibility:  visibility,
				TTL:         ttl,
			}

			// For small files, embed data in the header.
			if size >= 0 && size <= artifact.SmallArtifactThreshold {
				data, err := io.ReadAll(content)
				if err != nil {
					return fmt.Errorf("reading content: %w", err)
				}
				header.Data = data
				header.Size = int64(len(data))
				content = nil
			}

			response, err := client.Store(ctx, header, content)
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
			}

			fmt.Println(response.Ref)
			return nil
		},
	}
}

// --- fetch ---

func fetchCommand() *cli.Command {
	var socketPath, tokenPath *string
	var outputPath string

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("fetch", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.StringVarP(&outputPath, "output", "o", "", "output file path (default: stdout)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("ref argument required\n\nUsage: bureau artifact fetch <ref> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			result, err := client.Fetch(ctx, args[0])
			if err != nil {
				return err
			}
			defer result.Content.Close()

			var output io.Writer
			if outputPath != "" {
				file, err := os.Create(outputPath)
				if err != nil {
					return fmt.Errorf("creating output file: %w", err)
				}
				defer file.Close()
				output = file
			} else {
				output = os.Stdout
			}

			if _, err := io.Copy(output, result.Content); err != nil {
				return fmt.Errorf("writing content: %w", err)
			}
			return nil
		},
	}
}

// --- show ---

func showCommand() *cli.Command {
	var socketPath, tokenPath *string
	var jsonOutput bool

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("show", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("ref argument required\n\nUsage: bureau artifact show <ref> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			meta, err := client.Show(ctx, args[0])
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(meta)
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
			if meta.Visibility != "" {
				fmt.Fprintf(writer, "Visibility:\t%s\n", meta.Visibility)
			}
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

func existsCommand() *cli.Command {
	var socketPath, tokenPath *string
	var jsonOutput bool

	return &cli.Command{
		Name:    "exists",
		Summary: "Check whether an artifact exists",
		Usage:   "bureau artifact exists <ref> [flags]",
		Description: `Check if an artifact exists in the store. Exits 0 if it exists,
1 if it does not. With --json, outputs the exists response.`,
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("exists", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("ref argument required\n\nUsage: bureau artifact exists <ref> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.Exists(ctx, args[0])
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
			}

			if response.Exists {
				fmt.Printf("%s %s\n", response.Ref, formatSize(response.Size))
				return nil
			}

			// Not found — exit 1 for scripting. Print to stderr so
			// callers see what was checked, then return ExitError to
			// suppress the framework's redundant "error:" line.
			fmt.Fprintf(os.Stderr, "not found: %s\n", args[0])
			return &cli.ExitError{Code: 1}
		},
	}
}

// --- list ---

func listCommand() *cli.Command {
	var socketPath, tokenPath *string
	var contentType, label, cachePolicy, visibility string
	var minSize, maxSize int64
	var limit, offset int
	var jsonOutput bool

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.StringVar(&contentType, "content-type", "", "filter by content type")
			flagSet.StringVar(&label, "label", "", "filter by label")
			flagSet.StringVar(&cachePolicy, "cache-policy", "", "filter by cache policy")
			flagSet.StringVar(&visibility, "visibility", "", "filter by visibility")
			flagSet.Int64Var(&minSize, "min-size", 0, "minimum size in bytes")
			flagSet.Int64Var(&maxSize, "max-size", 0, "maximum size in bytes")
			flagSet.IntVar(&limit, "limit", 0, "maximum results (default: server decides)")
			flagSet.IntVar(&offset, "offset", 0, "skip this many results")
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.List(ctx, artifact.ListRequest{
				ContentType: contentType,
				Label:       label,
				CachePolicy: cachePolicy,
				Visibility:  visibility,
				MinSize:     minSize,
				MaxSize:     maxSize,
				Limit:       limit,
				Offset:      offset,
			})
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
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

func tagCommand() *cli.Command {
	var socketPath, tokenPath *string
	var optimistic bool
	var expectedPrevious string
	var jsonOutput bool

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("tag", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&optimistic, "optimistic", false, "overwrite existing tag without CAS check")
			flagSet.StringVar(&expectedPrevious, "expected", "", "expected current target hash (for CAS update)")
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("name and ref arguments required\n\nUsage: bureau artifact tag <name> <ref> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.Tag(ctx, args[0], args[1], optimistic, expectedPrevious)
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
			}

			fmt.Printf("%s → %s\n", response.Name, response.Ref)
			return nil
		},
	}
}

// --- resolve ---

func resolveCommand() *cli.Command {
	var socketPath, tokenPath *string
	var jsonOutput bool

	return &cli.Command{
		Name:    "resolve",
		Summary: "Resolve a ref or tag to a full hash",
		Usage:   "bureau artifact resolve <ref> [flags]",
		Description: `Resolve a short ref (art-<hex>), tag name, or full hash to the
canonical artifact reference. Useful for scripting: the output is
always the full ref.`,
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("resolve", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("ref argument required\n\nUsage: bureau artifact resolve <ref> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.Resolve(ctx, args[0])
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
			}

			fmt.Println(response.Ref)
			return nil
		},
	}
}

// --- tags ---

func tagsCommand() *cli.Command {
	var socketPath, tokenPath *string
	var prefix string
	var jsonOutput bool

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("tags", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.StringVar(&prefix, "prefix", "", "filter tags by name prefix")
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.Tags(ctx, prefix)
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
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

func deleteTagCommand() *cli.Command {
	var socketPath, tokenPath *string

	return &cli.Command{
		Name:    "delete-tag",
		Summary: "Delete a tag",
		Usage:   "bureau artifact delete-tag <name> [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("delete-tag", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("tag name required\n\nUsage: bureau artifact delete-tag <name> [flags]")
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
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

// pinToggleCommand builds either "pin" or "unpin" — the two commands
// differ only in name, help text, which client method is called, and
// the human-readable confirmation verb.
func pinToggleCommand(
	name, summary, verb string,
	call func(*artifact.Client, context.Context, string) (*artifact.PinResponse, error),
) *cli.Command {
	var socketPath, tokenPath *string
	var jsonOutput bool

	usage := fmt.Sprintf("bureau artifact %s <ref> [flags]", name)

	return &cli.Command{
		Name:    name,
		Summary: summary,
		Usage:   usage,
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet(name, pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("ref argument required\n\nUsage: %s", usage)
			}

			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := call(client, ctx, args[0])
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
			}

			fmt.Printf("%s: %s\n", verb, response.Ref)
			return nil
		},
	}
}

func pinCommand() *cli.Command {
	return pinToggleCommand("pin", "Pin an artifact (protect from GC)", "pinned",
		(*artifact.Client).Pin)
}

func unpinCommand() *cli.Command {
	return pinToggleCommand("unpin", "Unpin an artifact (allow GC)", "unpinned",
		(*artifact.Client).Unpin)
}

// --- gc ---

func gcCommand() *cli.Command {
	var socketPath, tokenPath *string
	var dryRun bool
	var jsonOutput bool

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("gc", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&dryRun, "dry-run", false, "report what would be collected without deleting")
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			ctx := context.Background()
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				return err
			}

			response, err := client.GC(ctx, dryRun)
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
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

func statusCommand() *cli.Command {
	var socketPath, tokenPath *string
	var jsonOutput bool

	return &cli.Command{
		Name:    "status",
		Summary: "Show artifact service status",
		Usage:   "bureau artifact status [flags]",
		Description: `Show service liveness information. This action does not require
authentication — it is a health check.`,
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("status", pflag.ContinueOnError)
			socketPath, tokenPath = connectFlags(flagSet)
			flagSet.BoolVar(&jsonOutput, "json", false, "output as JSON")
			return flagSet
		},
		Run: func(args []string) error {
			ctx := context.Background()
			// Status is unauthenticated — use token if available,
			// but don't fail if the token file is missing.
			client, err := connect(*socketPath, *tokenPath)
			if err != nil {
				// Fall back to unauthenticated.
				client = artifact.NewClientFromToken(*socketPath, nil)
			}

			response, err := client.Status(ctx)
			if err != nil {
				return err
			}

			if jsonOutput {
				return writeJSON(response)
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
