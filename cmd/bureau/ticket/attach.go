// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/artifactstore"
)

// --- attach ---

type attachParams struct {
	TicketConnection
	cli.JSONOutput
	Room              string `json:"room"              flag:"room,r"          desc:"room ID or alias (or use room-qualified ticket ref)"`
	Ticket            string `json:"ticket"             desc:"ticket ID"`
	ArtifactRef       string `json:"ref"                flag:"ref"             desc:"attach a pre-stored artifact by ref"`
	Label             string `json:"label"              flag:"label,l"         desc:"human-readable description"`
	ContentType       string `json:"content_type"       flag:"content-type"    desc:"MIME type of the artifact"`
	ArtifactSocket    string `json:"artifact_socket"    flag:"artifact-socket" desc:"artifact service socket path"`
	ArtifactTokenFile string `json:"artifact_token"     flag:"artifact-token"  desc:"artifact service token file path"`
}

func attachCommand() *cli.Command {
	var params attachParams

	return &cli.Command{
		Name:    "attach",
		Summary: "Attach an artifact to a ticket",
		Description: `Attach an artifact reference to a ticket. With a file argument,
the file is stored in the artifact service first, then the reference
is attached to the ticket. With --ref, attaches a pre-stored artifact
directly.

If the ticket already has an attachment with the same ref, the label
and content type are updated in place (upsert).

File-mode requires the artifact service to be reachable. Set
--artifact-socket and --artifact-token (or BUREAU_ARTIFACT_SOCKET
and BUREAU_ARTIFACT_TOKEN environment variables).`,
		Usage: "bureau ticket attach <ticket-id> [file] [flags]",
		Examples: []cli.Example{
			{
				Description: "Attach a file (stores as artifact, then attaches)",
				Command:     "bureau ticket attach tkt-a3f9 results.json",
			},
			{
				Description: "Attach a pre-stored artifact by ref",
				Command:     "bureau ticket attach tkt-a3f9 --ref art-abc123def456",
			},
			{
				Description: "Attach with a label and content type",
				Command:     "bureau ticket attach tkt-a3f9 trace.log --label 'stack trace' --content-type text/plain",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/ticket/attach"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// Parse positional arguments: <ticket-id> [file]
			switch len(args) {
			case 1:
				params.Ticket = args[0]
			case 2:
				params.Ticket = args[0]
				// args[1] is the file path, handled below.
			default:
				return cli.Validation("expected 1-2 positional arguments (ticket-id [file]), got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket attach <ticket-id> [file]")
			}

			// Determine the artifact ref: either from --ref or by storing a file.
			artifactRef := params.ArtifactRef
			contentType := params.ContentType

			if artifactRef == "" {
				// File mode: store the file first.
				if len(args) < 2 {
					return cli.Validation("either a file argument or --ref flag is required\n\nUsage: bureau ticket attach <ticket-id> <file>\n       bureau ticket attach <ticket-id> --ref <artifact-ref>")
				}
				filePath := args[1]

				storedRef, storedContentType, err := storeFileAsArtifact(ctx, filePath, contentType, params.ArtifactSocket, params.ArtifactTokenFile)
				if err != nil {
					return err
				}
				artifactRef = storedRef
				if contentType == "" {
					contentType = storedContentType
				}
			}

			// Attach the artifact to the ticket.
			ticketClient, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{
				"ticket": params.Ticket,
				"ref":    artifactRef,
			}
			if err := addResolvedRoom(ctx, fields, params.Room); err != nil {
				return err
			}
			if params.Label != "" {
				fields["label"] = params.Label
			}
			if contentType != "" {
				fields["content_type"] = contentType
			}

			var result mutationResult
			if err := ticketClient.Call(ctx, "add-attachment", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			logger.Info("artifact attached", "ticket", result.ID, "ref", artifactRef)
			return nil
		},
	}
}

// storeFileAsArtifact opens a file, stores it in the artifact service,
// and returns the artifact ref and resolved content type. Content type
// is inferred from the file extension when not explicitly provided.
func storeFileAsArtifact(ctx context.Context, filePath, contentType, artifactSocket, artifactTokenFile string) (string, string, error) {
	// Resolve artifact service connection.
	if artifactSocket == "" {
		artifactSocket = os.Getenv("BUREAU_ARTIFACT_SOCKET")
	}
	if artifactSocket == "" {
		return "", "", cli.Validation("artifact service socket is required for file-mode attach; set --artifact-socket or BUREAU_ARTIFACT_SOCKET")
	}
	if artifactTokenFile == "" {
		artifactTokenFile = os.Getenv("BUREAU_ARTIFACT_TOKEN")
	}
	if artifactTokenFile == "" {
		return "", "", cli.Validation("artifact service token is required for file-mode attach; set --artifact-token or BUREAU_ARTIFACT_TOKEN")
	}

	artifactClient, err := artifactstore.NewClient(artifactSocket, artifactTokenFile)
	if err != nil {
		return "", "", cli.Internal("creating artifact client: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", "", cli.Internal("opening %s: %w", filePath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", "", cli.Internal("stat %s: %w", filePath, err)
	}

	if contentType == "" {
		contentType = guessContentType(filePath)
	}

	header := &artifactstore.StoreHeader{
		ContentType: contentType,
		Filename:    filepath.Base(filePath),
		Size:        info.Size(),
	}

	// Embed small files directly in the header.
	var reader io.Reader
	if info.Size() <= artifactstore.SmallArtifactThreshold {
		data, readError := io.ReadAll(file)
		if readError != nil {
			return "", "", cli.Internal("reading %s: %w", filePath, readError)
		}
		header.Data = data
	} else {
		reader = file
	}

	response, err := artifactClient.Store(ctx, header, reader)
	if err != nil {
		return "", "", cli.Internal("storing artifact: %w", err)
	}

	return response.Ref, contentType, nil
}

// guessContentType infers MIME type from a filename extension.
// Falls back to "application/octet-stream" for unrecognized extensions.
func guessContentType(filename string) string {
	extension := strings.ToLower(filepath.Ext(filename))
	if extension == "" {
		return "application/octet-stream"
	}
	mimeType := mime.TypeByExtension(extension)
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}

// --- detach ---

type detachParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"ticket ID"`
	Ref    string `json:"ref"    desc:"artifact ref to remove"`
}

func detachCommand() *cli.Command {
	var params detachParams

	return &cli.Command{
		Name:    "detach",
		Summary: "Remove an artifact attachment from a ticket",
		Description: `Remove an artifact reference from a ticket's attachments. The
artifact itself is not deleted from the artifact service — only the
ticket's reference to it is removed.`,
		Usage: "bureau ticket detach <ticket-id> <artifact-ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Remove an attachment",
				Command:     "bureau ticket detach tkt-a3f9 art-abc123def456",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/ticket/attach"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 2 {
				params.Ticket = args[0]
				params.Ref = args[1]
			} else {
				return cli.Validation("expected 2 positional arguments (ticket-id artifact-ref), got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required")
			}
			if params.Ref == "" {
				return cli.Validation("artifact ref is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{
				"ticket": params.Ticket,
				"ref":    params.Ref,
			}
			if err := addResolvedRoom(ctx, fields, params.Room); err != nil {
				return err
			}

			var result mutationResult
			if err := client.Call(ctx, "remove-attachment", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			logger.Info("attachment removed", "ticket", result.ID, "ref", params.Ref)
			return nil
		},
	}
}
