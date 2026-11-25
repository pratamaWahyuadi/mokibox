package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/pratamaWahyuadi/mokibox/clients/dm"
)

type options struct {
	baseURL   string
	statePath string
	trustPath string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("a command is required")
	}
	opts := options{}
	global := flag.NewFlagSet("dmcli", flag.ContinueOnError)
	global.StringVar(&opts.baseURL, "base-url", envOrDefault("MOKIBOX_DM_BASE_URL", "http://localhost:8080"), "API base URL")
	global.StringVar(&opts.statePath, "state", envOrDefault("MOKIBOX_DM_STATE", "dm-state.json"), "encrypted state file")
	global.StringVar(&opts.trustPath, "trust", envOrDefault("MOKIBOX_DM_TRUST", "dm-trust.json"), "trust store file")
	if err := global.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := global.Args()
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a command is required")
	}
	command := args[0]
	commandArgs := args[1:]
	ctx := context.Background()
	switch command {
	case "init":
		return commandInit(ctx, opts, commandArgs)
	case "register":
		return commandRegister(ctx, opts, commandArgs)
	case "upload-keys":
		return commandUploadKeys(ctx, opts, commandArgs)
	case "fingerprint":
		return commandFingerprint(ctx, opts, commandArgs)
	case "verify":
		return commandVerify(ctx, opts, commandArgs)
	case "send":
		return commandSend(ctx, opts, commandArgs)
	case "poll":
		return commandPoll(ctx, opts, commandArgs)
	case "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dmcli [global flags] <command> [command flags]")
	fmt.Fprintln(os.Stderr, "commands: init, register, upload-keys, fingerprint, verify, send, poll")
	fmt.Fprintln(os.Stderr, "global flags: -base-url, -state, -trust")
}

func commandInit(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	userID := fs.String("user-id", "", "your MokiBox user UUID")
	generated := fs.Bool("generate-pickle-key", false, "generate and print a new pickle key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if _, err := uuid.Parse(*userID); err != nil {
		return fmt.Errorf("invalid -user-id: %w", err)
	}
	pickleKey, err := loadSecret("MOKIBOX_DM_PICKLE_KEY", "MOKIBOX_DM_PICKLE_KEY_FILE")
	generatedKey := ""
	if err != nil {
		if !*generated {
			return fmt.Errorf("pickle key is required: %w", err)
		}
		pickleKey, err = dm.GeneratePickleKey()
		if err != nil {
			return err
		}
		generatedKey = pickleKey
	}
	key, err := dm.ParsePickleKey(pickleKey)
	if err != nil {
		return err
	}
	client, err := dm.NewClient(opts.baseURL, "")
	if err != nil {
		return err
	}
	service, err := dm.NewService(client, dm.NewStore(opts.statePath), dm.NewTrust(opts.trustPath), key)
	if err != nil {
		return err
	}
	st, identity, err := service.Init(*userID)
	if err != nil {
		return err
	}
	if generatedKey != "" {
		fmt.Fprintln(os.Stderr, "SAVE THIS PICKLE KEY IN SECURE STORAGE; IT IS NOT WRITTEN TO DISK:")
		fmt.Fprintln(os.Stderr, generatedKey)
	}
	fmt.Printf("device_id: %s\n", st.DeviceID)
	fmt.Printf("identity_curve25519: %s\n", identity.Curve25519)
	fmt.Printf("identity_ed25519: %s\n", identity.Ed25519)
	return nil
}

func commandRegister(ctx context.Context, opts options, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	result, err := service.Register(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("device_id: %s\n", result.DeviceID)
	fmt.Printf("replaced_device: %t\n", result.ReplacedDevice)
	return nil
}

func commandUploadKeys(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("upload-keys", flag.ContinueOnError)
	count := fs.Uint("count", 50, "number of one-time keys to generate and upload")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	accepted, err := service.UploadKeys(ctx, *count)
	if err != nil {
		return err
	}
	fmt.Printf("accepted: %d\n", accepted)
	return nil
}

func commandFingerprint(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	userID := fs.String("user-id", "", "peer user UUID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if _, err := uuid.Parse(*userID); err != nil {
		return fmt.Errorf("invalid -user-id: %w", err)
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	info, err := service.PeerInfo(ctx, *userID)
	if err != nil {
		return err
	}
	fmt.Printf("user_id: %s\n", info.UserID)
	fmt.Printf("device_id: %s\n", info.DeviceID)
	fmt.Printf("fingerprint: %s\n", info.FormattedFingerprint)
	return nil
}

func commandVerify(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	userID := fs.String("user-id", "", "peer user UUID")
	deviceID := fs.String("device-id", "", "peer device UUID")
	fingerprint := fs.String("fingerprint", "", "fingerprint shown by the peer")
	force := fs.Bool("force", false, "rotate a previously verified fingerprint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	for name, value := range map[string]string{"user-id": *userID, "device-id": *deviceID, "fingerprint": *fingerprint} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if _, err := uuid.Parse(*userID); err != nil {
		return fmt.Errorf("invalid -user-id: %w", err)
	}
	if _, err := uuid.Parse(*deviceID); err != nil {
		return fmt.Errorf("invalid -device-id: %w", err)
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	var info dm.PeerInfo
	if *force {
		info, err = service.ForceVerify(ctx, *userID, *deviceID, *fingerprint)
	} else {
		info, err = service.Verify(ctx, *userID, *deviceID, *fingerprint)
	}
	if err != nil {
		return err
	}
	fmt.Printf("verified: %s / %s\n", info.UserID, info.DeviceID)
	fmt.Printf("fingerprint: %s\n", info.FormattedFingerprint)
	return nil
}

func commandSend(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	recipient := fs.String("to", "", "recipient user UUID")
	text := fs.String("text", "", "plaintext message")
	stdin := fs.Bool("stdin", false, "read plaintext from stdin")
	messageID := fs.String("message-id", "", "optional client message UUID for a retry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if _, err := uuid.Parse(*recipient); err != nil {
		return fmt.Errorf("invalid -to: %w", err)
	}
	plaintext := *text
	if plaintext == "" {
		if !*stdin {
			return fmt.Errorf("-text or -stdin is required")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read plaintext: %w", err)
		}
		plaintext = strings.TrimRight(string(raw), "\r\n")
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	result, err := service.Send(ctx, *recipient, plaintext, *messageID)
	if err != nil {
		return err
	}
	fmt.Printf("message_id: %s\n", result.MessageID)
	fmt.Printf("sequence: %d\n", result.Sequence)
	return nil
}

func commandPoll(ctx context.Context, opts options, args []string) error {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	limit := fs.Int("limit", 100, "maximum messages to poll")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	service, err := newService(opts)
	if err != nil {
		return err
	}
	messages, err := service.Receive(ctx, *limit)
	if err != nil {
		return err
	}
	for _, message := range messages {
		fmt.Printf("from: %s / %s\n", message.SenderUserID, message.SenderDeviceID)
		fmt.Printf("sequence: %d\n", message.Sequence)
		if message.AlreadyDelivered {
			fmt.Println("plaintext: <already delivered, awaiting acknowledgement>")
			continue
		}
		fmt.Printf("plaintext: %s\n", string(message.Plaintext))
	}
	return nil
}

func newService(opts options) (*dm.Service, error) {
	token, err := loadSecret("MOKIBOX_DM_TOKEN", "MOKIBOX_DM_TOKEN_FILE")
	if err != nil {
		return nil, err
	}
	pickleKeyValue, err := loadSecret("MOKIBOX_DM_PICKLE_KEY", "MOKIBOX_DM_PICKLE_KEY_FILE")
	if err != nil {
		return nil, err
	}
	pickleKey, err := dm.ParsePickleKey(pickleKeyValue)
	if err != nil {
		return nil, err
	}
	client, err := dm.NewClient(opts.baseURL, token)
	if err != nil {
		return nil, err
	}
	return dm.NewService(client, dm.NewStore(opts.statePath), dm.NewTrust(opts.trustPath), pickleKey)
}

func loadSecret(envName, fileEnvName string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value, nil
	}
	path := strings.TrimSpace(os.Getenv(fileEnvName))
	if path == "" {
		return "", fmt.Errorf("set %s or %s", envName, fileEnvName)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileEnvName, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("%s file is empty", fileEnvName)
	}
	return value, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
