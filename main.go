package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// version is reported by /api/status and `bridge version`.
const version = "0.1.0"

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bridge:", err)
		os.Exit(1)
	}
}

// rootCmd builds the bridge command tree.
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "bridge",
		Short:   "bridge — text your Claude Code agents from your phone",
		Version: version,
		Long: `bridge lets you text your Claude Code agents from your phone over your own
tailnet, with no third party in the path. Tell a running agent "use bridge so
we can text": it installs bridge, rehomes itself into a daemon-managed tmux
session, and hands you 'bridge attach' (keep a terminal on it) and 'bridge
pair' (put it on your phone).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		serveCmd(),
		connectCmd(),
		attachCmd(),
		pairCmd(),
		sendCmd(),
		exposeCmd(),
		hookCmd(),
		lockdownCmd(),
	)
	return root
}

// serveCmd runs the daemon.
func serveCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the bridge daemon (one per machine)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(port)
		},
	}
	cmd.Flags().IntVar(&port, "port", defaultPort, "TCP port to bind on 127.0.0.1")
	return cmd
}

// connectCmd is implemented in session.go; here it is a stub carrying the flag.
func connectCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Rehome the calling agent into the daemon and register it",
		Long: `Rehome the calling agent into the daemon and register it.

Written for the agent about to run it — because that's who runs it:

WHAT HAPPENS TO YOU. Nothing is lost. connect finds THIS conversation (your
own session id, from the environment) and resumes it inside a tmux window
the daemon manages. Same conversation, same memory, same working directory —
you continue mid-thought. The terminal you are in right now becomes a
retired copy: it prints a sign-off and asks your human to close it. You are
not being replaced; you are moving house.

WHAT YOU GAIN. Your human can text you from their phone (and approve your
permission prompts from anywhere), reach you in a terminal with 'bridge
attach <your-name>', and you can message other registered agents with
'bridge send --to <name>'. If the daemon restarts, it revives you.

WHAT TO DO. Run: bridge connect --name <the-name-you-answer-to>
(omit --name for a generated one). Then tell your human what you ran and
that they can close this old terminal. That's the whole ceremony.

Your name may get a numeric suffix if another live agent already answers
to it — the daemon tells you the final address.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionCmdImpls["connect"](&cliCtx{args: args, name: name})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the address this agent answers to")
	return cmd
}

// attachCmd is implemented in session.go; here it is a stub.
func attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <name>",
		Short: "Attach a terminal to a managed agent (tmux attach)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionCmdImpls["attach"](&cliCtx{args: args})
		},
	}
}

// sendCmd is implemented in session.go; here it is a stub carrying the flag.
func sendCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "send <text>",
		Short: "Send a message from this agent (to the phone, or --to another agent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionCmdImpls["send"](&cliCtx{args: args, to: to})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "deliver to another agent by name instead of the phone")
	return cmd
}

// exposeCmd is implemented in session.go; here it is a stub.
func exposeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "expose",
		Short: "Publish the daemon to your tailnet via tailscale serve",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionCmdImpls["expose"](&cliCtx{args: args})
		},
	}
}

// hookCmd is implemented in session.go; here it is a stub.
func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hook",
		Short: "Claude Code Notification hook shim (POSTs the event to the daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionCmdImpls["hook"](&cliCtx{args: args})
		},
	}
}

// pairCmd asks the running daemon to mint a one-time code and prints it.
func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair",
		Short: "Print a one-time pairing code for your phone (valid 10 minutes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Code string `json:"code"`
			}
			if err := daemonRequest(http.MethodPost, "/local/pair", nil, &resp); err != nil {
				return err
			}
			fmt.Printf("Pairing code: %s  (valid 10 minutes)\n", resp.Code)
			return nil
		},
	}
}

// lockdownCmd tells the running daemon to revoke every device and shut down.
func lockdownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lockdown",
		Short: "Revoke every paired device and stop the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := daemonRequest(http.MethodPost, "/local/lockdown", nil, nil); err != nil {
				return err
			}
			fmt.Println("Locked down: all devices revoked, daemon stopping.")
			return nil
		},
	}
}

// daemonRequest calls the running daemon's local API, authenticating with the
// lockfile token. out, when non-nil, receives the decoded JSON response.
func daemonRequest(method, path string, body, out any) error {
	lf, err := readLockfile()
	if err != nil {
		return fmt.Errorf("bridge daemon not running (no lockfile): %w", err)
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", lf.Port, path), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+lf.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach bridge daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
