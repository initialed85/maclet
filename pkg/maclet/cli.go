package maclet

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	version                  = "0.1.0-dev"
	defaultStateDir          = ".maclet"
	fallbackNodeName         = "maclet"
	defaultVXLANPort         = 8472
	defaultVXLANMTU          = 1450
	defaultClusterCIDR       = "10.42.0.0/16"
	defaultServiceCIDR       = "10.43.0.0/16"
	defaultMaxPods           = 110
	defaultLeaseDurationSecs = 40
	defaultHeartbeat         = 10 * time.Second
	defaultDrainTimeout      = 10 * time.Second
	defaultKubeletPort       = 10250
)

var errNotFound = errors.New("resource not found")

func defaultStatePath() string {
	home := homeDirectory()
	if home == "" {
		return defaultStateDir
	}
	return filepath.Join(home, defaultStateDir)
}

func usage() {
	fmt.Fprintf(os.Stderr, `maclet %s

Usage:
  maclet join [options]                 register/heartbeat a Darwin node
  maclet leave [options]                unregister the node and remove local state
  maclet workloads [options]            print pods scheduled to this node as JSON
  maclet cleanup-controller [options]   remove stale native Pods with explicit cleanup RBAC
  maclet version

Run "maclet join --help", "maclet leave --help", or "maclet workloads --help" for command options.
`, version)
}

func runJoinCommand(args []string) error {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cfg := JoinConfig{}
	flags.StringVar(&cfg.Server, "server", "", "Kubernetes/K3s API URL (https://host:6443)")
	flags.StringVar(&cfg.Token, "token", "", "K3s join token (prefer --token-file to avoid process listings)")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "read K3s join token from this file, or - for stdin")
	flags.StringVar(&cfg.NodeName, "node-name", "", "Kubernetes node name (defaults to the local hostname)")
	flags.StringVar(&cfg.NodeIP, "node-ip", "", "node/underlay IP advertised to Kubernetes (auto-detected if empty)")
	flags.StringVar(&cfg.ExternalIP, "external-ip", "", "Kubernetes ExternalIP address (defaults to --vxlan-local or --node-ip)")
	flags.StringVar(&cfg.StateDir, "state-dir", defaultStatePath(), "maclet state directory")
	flags.BoolVar(&cfg.InsecureSkipTLSVerify, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	flags.BoolVar(&cfg.Once, "once", false, "register, heartbeat once, and exit")
	flags.StringVar(&cfg.VXLANBinary, "vxlan-binary", "", "path to darwin-vxlan; start it after PodCIDR assignment")
	flags.StringVar(&cfg.MackerBinary, "macker-binary", "", "path to Macker; start assigned native Pods through it (defaults to PATH)")
	flags.BoolVar(&cfg.Debug, "debug", false, "log generated Macker invocations and native workload decisions (may include environment values)")
	flags.BoolVar(&cfg.DNSResolver, "dns-resolver", true, "configure macOS /etc/resolver/cluster.local to use the cluster DNS Service")
	flags.StringVar(&cfg.VXLANRemote, "vxlan-remote", "", "VXLAN remote underlay address")
	flags.StringVar(&cfg.VXLANLocal, "vxlan-local", "", "VXLAN local underlay address (defaults to --node-ip)")
	flags.StringVar(&cfg.VXLANGatewayMAC, "vxlan-gateway-mac", "", "static remote flannel.1 MAC override (normally discovered through the K3s controller client)")
	flags.StringVar(&cfg.PeerKubeconfig, "peer-kubeconfig", "", "optional kubeconfig override for peer Flannel discovery and privileged cleanup")
	flags.StringVar(&cfg.PeerContext, "peer-context", "", "kubeconfig context used for peer discovery (defaults to the current context)")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", defaultVXLANPort, "VXLAN UDP port")
	flags.IntVar(&cfg.VXLANMTU, "vxlan-mtu", defaultVXLANMTU, "VXLAN bridge MTU")
	flags.StringVar(&cfg.ClusterCIDR, "cluster-cidr", defaultClusterCIDR, "cluster Pod network CIDR routed through the Darwin VXLAN")
	flags.StringVar(&cfg.ServiceCIDR, "service-cidr", defaultServiceCIDR, "Kubernetes Service network CIDR routed through the Darwin VXLAN")
	flags.DurationVar(&cfg.DrainTimeout, "drain-timeout", defaultDrainTimeout, "maximum time for API cordon during graceful shutdown")
	if err := flags.Parse(args); err != nil {
		return err
	}
	flags.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "cluster-cidr":
			cfg.clusterCIDRSet = true
		case "service-cidr":
			cfg.serviceCIDRSet = true
		}
	})
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runJoin(cfg)
}

func runWorkloadsCommand(args []string) error {
	flags := flag.NewFlagSet("workloads", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := defaultStatePath()
	insecure := false
	flags.StringVar(&stateDir, "state-dir", stateDir, "maclet state directory")
	flags.BoolVar(&insecure, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return runWorkloads(stateDir, insecure)
}

// Main runs the maclet command-line interface and returns the process exit code.
func Main(args []string) int {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(args) == 0 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "join":
		err = runJoinCommand(args[1:])
	case "leave":
		err = leaveCommand(args[1:])
	case "workloads":
		err = runWorkloadsCommand(args[1:])
	case "cleanup-controller":
		err = cleanupControllerCommand(args[1:])
	case "resolver-helper":
		err = resolverHelperCommand(args[1:])
	case "version":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		log.Printf("error: %v", err)
		return 1
	}
	return 0
}
