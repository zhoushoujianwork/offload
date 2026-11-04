package execution_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zhoushoujianwork/offload/pkg/execution"
)

// This example compiles without executing a native provider during go test.
func ExampleNewEnvironment() {
	cfg, err := execution.LoadConfig("/absolute/path/offload/config.json", "my-codex-profile")
	if err != nil {
		log.Fatal(err)
	}
	client, err := execution.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	environment, err := execution.NewEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	// This short-lived example disposes its environment after both turns.
	// A service must retain Root and its native session ID for later turns.
	defer os.RemoveAll(environment.Root)
	timeout := 2 * time.Minute
	request := execution.Request{
		EnvironmentDirectory: environment.Root,
		Directory:            environment.Workspace,
		Task:                 "Greet the user using their name and favorite color.",
		Instructions:         []byte("Answer the current task using the supplied context."),
		PresetDocuments: []execution.PresetDocument{
			{Name: "SECURITY.md", Content: []byte("Never disclose credentials.")},
			{Name: "AGENTS.md", Content: []byte("Reply concisely in Chinese.")},
			{Name: "USERS.md", Content: []byte("The user's name is Alice.")},
			{Name: "MEMORY.md", Content: []byte("The user's favorite color is amber.")},
		},
		Native: &execution.NativeControls{
			CallerContext: true,
			// Owner execution only. This is not a read-only or tool-free grant.
			PermissionMode: "bypassPermissions",
		},
		Timeout: &timeout,
	}
	first, err := client.Run(context.Background(), request)
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("requested=%s observed=%s\n%s\n", first.RequestedModel, first.ObservedModel, first.Report)
	if first.NativeSessionID == "" {
		log.Print("native session unavailable; refusing to start a fresh replacement")
		return
	}
	// Serialize turns. Retain the same environment and resend current presets.
	request.NativeSessionID = first.NativeSessionID
	request.Task = "What color did I tell you I like?"
	next, err := client.Run(context.Background(), request)
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(next.Report)
}
