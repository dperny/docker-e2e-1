package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types/swarm"
	"github.com/spf13/cobra"

	"github.com/docker/docker-e2e/testkit/machines"
)

var createCmd = &cobra.Command{
	Use:   "create <linux_count> <windows_count>",
	Short: "Provision a test environment",
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, err := cmd.Flags().GetBool("debug")
		if err != nil {
			return err
		}
		if debug {
			log.SetLevel(log.DebugLevel)
		}
		if len(args) == 0 {
			return errors.New("Config missing")
		}

		linuxCount, err := strconv.Atoi(args[0])
		if err != nil {
			log.Fatal(err)
		}
		windowsCount, err := strconv.Atoi(args[1])
		if err != nil {
			log.Fatal(err)
		}

		lm, wm, err := machines.GetTestMachines(linuxCount, windowsCount)
		if err != nil {
			log.Fatalf("Failure: %s", err)
		}
		noInit, err := cmd.Flags().GetBool("no-swarm")
		if err != nil {
			return err
		}
		listenAddr, _ := cmd.Flags().GetString("listen-addr")
		machines := append(lm, wm...)
		if !noInit {
			// Init and join
			cli, err := machines[0].GetEngineAPI()
			if err != nil {
				return err
			}
			internalIP, err := machines[0].GetInternalIP()
			if err != nil {
				return err
			}
			log.Debugf("Initializing swarm on %s", machines[0].GetName())
			_, err = cli.SwarmInit(context.TODO(), swarm.InitRequest{
				ListenAddr:    listenAddr,
				AdvertiseAddr: internalIP,
			})
			if err != nil {
				return err
			}
			swarmInfo, err := cli.SwarmInspect(context.TODO())
			if err != nil {
				return err
			}
			info, err := cli.Info(context.TODO())
			if err != nil {
				return err
			}
			for _, m := range machines[1:] {
				log.Debugf("Joining %s as worker", m.GetName())
				cliW, err := m.GetEngineAPI()
				if err != nil {
					return err
				}
				err = cliW.SwarmJoin(context.TODO(), swarm.JoinRequest{
					ListenAddr:  listenAddr,
					RemoteAddrs: []string{info.Swarm.RemoteManagers[0].Addr},
					JoinToken:   swarmInfo.JoinTokens.Worker,
				})
				if err != nil {
					return err
				}
			}
		}
		for _, m := range machines {
			fmt.Println(m.GetConnectionEnv())
			fmt.Println("")
		}
		return nil
	},
}

func init() {
	createCmd.Flags().BoolP("debug", "d", false, "enable verbose logging")
	createCmd.Flags().BoolP("no-swarm", "n", false, "skip swarm init and join")
	createCmd.Flags().String("listen-addr", "0.0.0.0:2377", "passed to swarm init and join")
}
