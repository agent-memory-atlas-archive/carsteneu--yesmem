package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/opencode"
)

func TestOpencodeServerConnectionRejectsDaemonOwnedServer(t *testing.T) {
	group := "/user.slice/user@1000.service/app.slice/yesmem.service"
	for _, server := range []string{group, group + "/child"} {
		args, env, err := opencodeServerConnection(opencode.Service{PID: 42}, group, server)
		if err == nil || len(args) != 0 || len(env) != 0 {
			t.Fatalf("daemon-owned server must not be used: args=%v err=%v", args, err)
		}
	}
}

func TestOpencodeServerConnectionAttachesWithoutPuttingPasswordInArgv(t *testing.T) {
	svc := opencode.Service{PID: 42, URL: "http://127.0.0.1:49374", Password: "private-password"}
	args, env, err := opencodeServerConnection(svc,
		"/user.slice/app.slice/yesmem.service", "/user.slice/app.slice/opencode.service")
	if err != nil || strings.Join(args, " ") != "--server "+svc.URL {
		t.Fatalf("connection args=%v err=%v", args, err)
	}
	if strings.Contains(strings.Join(args, " "), svc.Password) {
		t.Fatal("server password exposed in argv")
	}
	if len(env) != 1 || env[0] != "OPENCODE_PASSWORD="+svc.Password {
		t.Fatal("explicit connection must carry authentication")
	}
}

func TestServiceControlGroupDistinguishesInteractiveScope(t *testing.T) {
	if !serviceControlGroup("/user.slice/user@1000.service/app.slice/yesmem.service") {
		t.Fatal("daemon service not recognized")
	}
	if serviceControlGroup("/user.slice/user@1000.service/app.slice/terminal.scope") {
		t.Fatal("interactive scope confused with its ancestor service")
	}
}
