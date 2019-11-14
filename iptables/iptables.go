package iptables

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// RedirectAllMode indicates redirecting all ports.
	RedirectAllMode = "redirect-all"

	// RedirectListedMode indicates redirecting a given list of ports.
	RedirectListedMode = "redirect-listed"

	// IptablesPreroutingChainName specifies an iptables `PREROUTING` chain,
	// responsible for packets that just arrived at the network interface.
	IptablesPreroutingChainName = "PREROUTING"

	// IptablesOutputChainName specifies an iptables `OUTPUT` chain.
	IptablesOutputChainName = "OUTPUT"

	outputChainName   = "PROXY_INIT_OUTPUT"
	redirectChainName = "PROXY_INIT_REDIRECT"
)

var (
	// ExecutionTraceID provides a unique identifier for this script's execution.
	ExecutionTraceID = strconv.Itoa(int(time.Now().Unix()))

	sectionDelimiter = strings.Repeat("-", 60)
)

// FirewallConfiguration specifies how to configure a pod's iptables.
type FirewallConfiguration struct {
	Mode                   string
	PortsToRedirectInbound []int
	InboundPortsToIgnore   []int
	OutboundPortsToIgnore  []int
	ProxyInboundPort       int
	ProxyOutgoingPort      int
	ProxyUID               int
	SimulateOnly           bool
	NetNs                  string
	UseWaitFlag            bool
}

//ConfigureFirewall configures a pod's internal iptables to redirect all desired traffic through the proxy, allowing for
// the pod to join the service mesh. A lot of this logic was based on
// https://github.com/istio/istio/blob/e83411e/pilot/docker/prepare_proxy.sh
func ConfigureFirewall(firewallConfiguration FirewallConfiguration) error {

	fmt.Printf("Tracing this script execution as [%s]\n", ExecutionTraceID)

	startSection("current state")
	if err := executeCommand(firewallConfiguration, makeShowAllRules()); err != nil {
		fmt.Println("Aborting firewall configuration")
		return err
	}
	endSection()

	startSection("cleanup")
	// cleanup rules before adding new ones in
	_ = executeCommand(
		firewallConfiguration,
		makeJumpFromChainToAnotherForAllProtocols(
			IptablesOutputChainName,
			outputChainName,
			"install-proxy-init-prerouting",
			true))
	_ = executeCommand(
		firewallConfiguration,
		makeJumpFromChainToAnotherForAllProtocols(
			IptablesPreroutingChainName,
			redirectChainName,
			"install-proxy-init-prerouting",
			true))

	for _, chain := range []string{outputChainName, redirectChainName} {
		_ = executeCommand(firewallConfiguration, makeFlushChain(chain))
		_ = executeCommand(firewallConfiguration, makeDeleteChain(chain))
	}
	endSection()

	commands := make([]*exec.Cmd, 0)

	startSection("configuration")

	commands = addIncomingTrafficRules(commands, firewallConfiguration)

	commands = addOutgoingTrafficRules(commands, firewallConfiguration)

	endSection()

	startSection("adding rules")

	for _, cmd := range commands {
		if err := executeCommand(firewallConfiguration, cmd); err != nil {
			fmt.Println("Aborting firewall configuration")
			return err
		}
	}

	endSection()

	startSection("end state")
	_ = executeCommand(firewallConfiguration, makeShowAllRules())
	endSection()

	return nil
}

//formatComment is used to format iptables comments in such way that it is possible to identify when the rules were added.
// This helps debug when iptables has some stale rules from previous runs, something that can happen frequently on minikube.
func formatComment(text string) string {
	return fmt.Sprintf("proxy-init/%s/%s", text, ExecutionTraceID)
}

func startSection(text string) {
	fmt.Printf("%s\n%s\n", text, sectionDelimiter)
}

func endSection() {
	fmt.Printf("\n\n")
}

func addOutgoingTrafficRules(commands []*exec.Cmd, firewallConfiguration FirewallConfiguration) []*exec.Cmd {
	commands = append(commands, makeCreateNewChain(outputChainName, "redirect-common-chain"))

	// Ignore traffic from the proxy
	if firewallConfiguration.ProxyUID > 0 {
		fmt.Printf("Ignoring uid %d\n", firewallConfiguration.ProxyUID)
		// Redirect calls originating from the proxy destined for an app container e.g. app -> proxy(outbound) -> proxy(inbound) -> app
		commands = append(commands, makeRedirectChainForOutgoingTraffic(outputChainName, redirectChainName, firewallConfiguration.ProxyUID, "redirect-non-loopback-local-traffic"))
		commands = append(commands, makeIgnoreUserID(outputChainName, firewallConfiguration.ProxyUID, "ignore-proxy-user-id"))
	} else {
		fmt.Println("Not ignoring any uid")
	}

	// Ignore loopback
	commands = append(commands, makeIgnoreLoopback(outputChainName, "ignore-loopback"))
	// Ignore ports
	commands = addRulesForIgnoredPorts(firewallConfiguration.OutboundPortsToIgnore, outputChainName, commands)

	fmt.Printf("Redirecting all OUTPUT to %d\n", firewallConfiguration.ProxyOutgoingPort)
	commands = append(commands, makeRedirectChainToPort(outputChainName, firewallConfiguration.ProxyOutgoingPort, "redirect-all-outgoing-to-proxy-port"))

	//Redirect all remaining outbound traffic to the proxy.
	commands = append(
		commands,
		makeJumpFromChainToAnotherForAllProtocols(
			IptablesOutputChainName,
			outputChainName,
			"install-proxy-init-output",
			false))

	return commands
}

func addIncomingTrafficRules(commands []*exec.Cmd, firewallConfiguration FirewallConfiguration) []*exec.Cmd {
	commands = append(commands, makeCreateNewChain(redirectChainName, "redirect-common-chain"))
	commands = addRulesForIgnoredPorts(firewallConfiguration.InboundPortsToIgnore, redirectChainName, commands)
	commands = addRulesForInboundPortRedirect(firewallConfiguration, redirectChainName, commands)

	//Redirect all remaining inbound traffic to the proxy.
	commands = append(
		commands,
		makeJumpFromChainToAnotherForAllProtocols(
			IptablesPreroutingChainName,
			redirectChainName,
			"install-proxy-init-prerouting",
			false))

	return commands
}

func addRulesForInboundPortRedirect(firewallConfiguration FirewallConfiguration, chainName string, commands []*exec.Cmd) []*exec.Cmd {
	if firewallConfiguration.Mode == RedirectAllMode {
		fmt.Println("Will redirect all INPUT ports to proxy")
		//Create a new chain for redirecting inbound and outbound traffic to the proxy port.
		commands = append(commands, makeRedirectChainToPort(chainName,
			firewallConfiguration.ProxyInboundPort,
			"redirect-all-incoming-to-proxy-port"))

	} else if firewallConfiguration.Mode == RedirectListedMode {
		fmt.Printf("Will redirect some INPUT ports to proxy: %v\n", firewallConfiguration.PortsToRedirectInbound)
		for _, port := range firewallConfiguration.PortsToRedirectInbound {
			commands = append(
				commands,
				makeRedirectChainToPortBasedOnDestinationPort(
					chainName,
					port,
					firewallConfiguration.ProxyInboundPort,
					fmt.Sprintf("redirect-port-%d-to-proxy-port", port)))
		}
	}
	return commands
}

func addRulesForIgnoredPorts(portsToIgnore []int, chainName string, commands []*exec.Cmd) []*exec.Cmd {
	for _, ignoredPort := range portsToIgnore {
		fmt.Printf("Will ignore port %d on chain %s\n", ignoredPort, chainName)

		commands = append(commands, makeIgnorePort(chainName, ignoredPort, fmt.Sprintf("ignore-port-%d", ignoredPort)))
	}
	return commands
}

func executeCommand(firewallConfiguration FirewallConfiguration, cmd *exec.Cmd) error {
	if firewallConfiguration.UseWaitFlag {
		fmt.Println("Setting UseWaitFlag: iptables will wait for xtables to become available")
		cmd.Args = append(cmd.Args, "-w")
	}

	if firewallConfiguration.SimulateOnly {
		return nil
	}

	// wrap up the cmd with nsenter if we were givin a netns
	if len(firewallConfiguration.NetNs) > 0 {
		cmd.Args = append([]string{
			"nsenter",
			"--net", firewallConfiguration.NetNs,
		}, cmd.Args...)
	}

	fmt.Printf(":; %s\n", strings.Trim(fmt.Sprintf("%v", cmd.Args), "[]"))

	out, err := cmd.CombinedOutput()

	if len(out) > 0 {
		fmt.Printf("%s\n", out)
	}

	if err != nil {
		return err
	}

	return nil
}

func makeIgnoreUserID(chainName string, uid int, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-m", "owner",
		"--uid-owner", strconv.Itoa(uid),
		"-j", "RETURN",
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeCreateNewChain(name string, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-N", name,
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeFlushChain(name string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-F", name)
}

func makeDeleteChain(name string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-X", name)
}

func makeRedirectChainToPort(chainName string, portToRedirect int, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-p", "tcp",
		"-j", "REDIRECT",
		"--to-port", strconv.Itoa(portToRedirect),
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeIgnorePort(chainName string, portToIgnore int, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-p", "tcp",
		"--destination-port", strconv.Itoa(portToIgnore),
		"-j", "RETURN",
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeIgnoreLoopback(chainName string, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-o", "lo",
		"-j", "RETURN",
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeRedirectChainToPortBasedOnDestinationPort(chainName string, destinationPort int, portToRedirect int, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-p", "tcp",
		"--destination-port", strconv.Itoa(destinationPort),
		"-j", "REDIRECT",
		"--to-port", strconv.Itoa(portToRedirect),
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeJumpFromChainToAnotherForAllProtocols(
	chainName string, targetChain string, comment string, delete bool) *exec.Cmd {
	action := "-A"
	if delete {
		action = "-D"
	}

	return exec.Command("iptables",
		"-t", "nat",
		action, chainName,
		"-j", targetChain,
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeRedirectChainForOutgoingTraffic(chainName string, redirectChainName string, uid int, comment string) *exec.Cmd {
	return exec.Command("iptables",
		"-t", "nat",
		"-A", chainName,
		"-m", "owner",
		"--uid-owner", strconv.Itoa(uid),
		"-o", "lo",
		"!", "-d 127.0.0.1/32",
		"-j", redirectChainName,
		"-m", "comment",
		"--comment", formatComment(comment))
}

func makeShowAllRules() *exec.Cmd {
	return exec.Command("iptables-save")
}
