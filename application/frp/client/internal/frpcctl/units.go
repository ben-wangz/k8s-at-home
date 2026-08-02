package frpcctl

import (
	"fmt"
	"io"
	"os/exec"
)

func renderQuadlet(image, secretName string) string {
	return fmt.Sprintf(`[Unit]
Description=frp client container
Documentation=https://gofrp.org/
Wants=network-online.target
After=network-online.target

[Container]
Image=%s
ContainerName=frpc
Network=host
Volume=/etc/frp/frpc.toml:/etc/frp/frpc.toml:ro
Secret=%s,type=mount,target=frp-auth,uid=65532,gid=65532,mode=0400
Exec=-c /etc/frp/frpc.toml
Pull=missing
ReadOnly=true
NoNewPrivileges=true
DropCapability=all
User=65532:65532
PodmanArgs=--memory=128m --cpus=0.5 --pids-limit=128 --log-driver=journald

[Service]
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, image, secretName)
}

func renderMonitorService() string {
	return `[Unit]
Description=Independent frp end-to-end monitor
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/frpcctl monitor --config /etc/frp-monitor/config.json
SuccessExitStatus=1
User=root
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=read-only
ProtectSystem=strict
ReadWritePaths=/var/lib/frp-monitor
`
}

func renderMonitorTimer() string {
	return `[Unit]
Description=Run the independent frp monitor every 15 minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=15min
Persistent=true
RandomizedDelaySec=30
Unit=frp-monitor.service

[Install]
WantedBy=timers.target
`
}

func runCommand(stdin io.Reader, stdout, stderr io.Writer, name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}
