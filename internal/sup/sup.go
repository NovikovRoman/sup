package sup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/goware/prefixer"
	"golang.org/x/crypto/ssh"

	"github.com/DTreshy/sup/internal/command"
	"github.com/DTreshy/sup/internal/envs"
	"github.com/DTreshy/sup/internal/network"
	"github.com/DTreshy/sup/internal/supfile"
	"github.com/DTreshy/sup/pkg/colors"
)

const VERSION = "0.5"

type Stackup struct {
	conf   *supfile.Supfile
	debug  bool
	prefix bool
}

func New(conf *supfile.Supfile) (*Stackup, error) {
	return &Stackup{
		conf: conf,
	}, nil
}

// Run runs set of commands on multiple hosts defined by network sequentially.
// TODO: This megamoth method needs a big refactor and should be split
//
//	to multiple smaller methods.
func (sup *Stackup) Run(net *network.Network, envVars envs.EnvList, commands ...*command.Command) error {
	if len(commands) == 0 {
		return errors.New("no commands to be run")
	}

	env := envVars.AsExport()

	// Create clients for every host (either SSH or Localhost).
	var bastion *SSHClient

	if net.Bastion != "" {
		bastion = &SSHClient{}
		if err := bastion.Connect(net.Bastion); err != nil {
			return errors.Join(err, errors.New("connecting to bastion failed"))
		}
	}

	var wg sync.WaitGroup

	clientCh := make(chan Client, len(net.Hosts))
	errCh := make(chan error, len(net.Hosts))

	for i, host := range net.Hosts {
		wg.Add(1)

		go func(i int, host string) {
			defer wg.Done()

			// Localhost client.
			if host == "localhost" {
				local := &LocalhostClient{
					env: env + `export SUP_HOST="` + host + `";`,
				}
				if err := local.Connect(host); err != nil {
					errCh <- errors.Join(err, errors.New("connecting to localhost failed"))
					return
				}

				clientCh <- local

				return
			}

			// SSH client.
			remote := &SSHClient{
				env:   env + `export SUP_HOST="` + host + `";`,
				user:  net.User,
				color: colors.Colors[i%len(colors.Colors)],
			}

			if bastion != nil {
				if err := remote.ConnectWith(host, bastion.DialThrough); err != nil {
					errCh <- errors.Join(err, errors.New("connecting to remote host through bastion failed"))
					return
				}
			} else {
				if err := remote.Connect(host); err != nil {
					errCh <- errors.Join(err, errors.New("connecting to remote host failed"))
					return
				}
			}
			clientCh <- remote
		}(i, host)
	}

	wg.Wait()
	close(clientCh)
	close(errCh)

	maxLen := 0

	var clients []Client

	for client := range clientCh {
		_, prefixLen := client.Prefix()
		if prefixLen > maxLen {
			maxLen = prefixLen
		}

		clients = append(clients, client)
	}

	defer closeRemotes(clients)

	for err := range errCh {
		return errors.Join(err, errors.New("connecting to clients failed"))
	}

	// Run command or run multiple commands defined by target sequentially.
	for _, cmd := range commands {
		// Translate command into task(s).
		tasks, err := sup.createTasks(cmd, clients, env)
		if err != nil {
			return errors.Join(err, errors.New("creating task failed"))
		}

		// Run tasks sequentially.
		for _, task := range tasks {
			var (
				writers []io.Writer
				wg      sync.WaitGroup
			)

			// Run tasks on the provided clients.
			for _, c := range task.Clients {
				var (
					prefix    string
					prefixLen int
				)

				if sup.prefix {
					prefix, prefixLen = c.Prefix()
					if len(prefix) < maxLen { // Left padding.
						prefix = strings.Repeat(" ", maxLen-prefixLen) + prefix
					}
				}

				err := c.Run(task)
				if err != nil {
					return errors.Join(err, errors.New(prefix+"task failed"))
				}

				// Copy over tasks's STDOUT.
				wg.Add(1)

				go func(c Client) {
					defer wg.Done()

					_, err := io.Copy(os.Stdout, prefixer.New(c.Stdout(), prefix))
					if err != nil && err != io.EOF {
						// TODO: io.Copy() should not return io.EOF at all.
						// Upstream bug? Or prefixer.WriteTo() bug?
						fmt.Fprintf(os.Stderr, "%v", errors.Join(err, errors.New(prefix+"reading STDOUT failed")))
					}
				}(c)

				// Copy over tasks's STDERR.
				wg.Add(1)

				go func(c Client) {
					defer wg.Done()

					_, err := io.Copy(os.Stderr, prefixer.New(c.Stderr(), prefix))
					if err != nil && err != io.EOF {
						fmt.Fprintf(os.Stderr, "%v", errors.Join(err, errors.New(prefix+"reading STDERR failed")))
					}
				}(c)

				writers = append(writers, c.Stdin())
			}

			// Copy over task's STDIN.
			if task.Input != nil {
				go func() {
					writer := io.MultiWriter(writers...)

					_, err := io.Copy(writer, task.Input)
					if err != nil && err != io.EOF {
						fmt.Fprintf(os.Stderr, "%v", errors.Join(err, errors.New("copying STDIN failed")))
					}
					// TODO: Use MultiWriteCloser (not in Stdlib), so we can writer.Close() instead?
					for _, c := range clients {
						c.WriteClose()
					}
				}()
			}

			// Catch OS signals and pass them to all active clients.
			trap := make(chan os.Signal, 1)

			signal.Notify(trap, os.Interrupt)

			go func() {
				for {
					sig, ok := <-trap
					if !ok {
						return
					}

					for _, c := range task.Clients {
						err := c.Signal(sig)
						if err != nil {
							fmt.Fprintf(os.Stderr, "%v", errors.Join(err, errors.New("sending signal failed")))
						}
					}
				}
			}()

			// Wait for all I/O operations first.
			wg.Wait()

			// Make sure each client finishes the task, return on failure.
			for _, c := range task.Clients {
				wg.Add(1)

				go func(c Client) {
					defer wg.Done()

					if err := c.Wait(); err != nil {
						var prefix string

						if sup.prefix {
							var prefixLen int

							prefix, prefixLen = c.Prefix()

							if len(prefix) < maxLen { // Left padding.
								prefix = strings.Repeat(" ", maxLen-prefixLen) + prefix
							}
						}

						if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() != 15 {
							// TODO: Store all the errors, and print them after Wait().
							fmt.Fprintf(os.Stderr, "%s%v\n", prefix, e)
							os.Exit(e.ExitStatus())
						}

						fmt.Fprintf(os.Stderr, "%s%v\n", prefix, err)

						// TODO: Shouldn't os.Exit(1) here. Instead, collect the exit statuses for later.
						os.Exit(1)
					}
				}(c)
			}

			// Wait for all commands to finish.
			wg.Wait()

			// Stop catching signals for the currently active clients.
			signal.Stop(trap)
			close(trap)
		}
	}

	return nil
}

func closeRemotes(clients []Client) {
	for _, client := range clients {
		if remote, ok := client.(*SSHClient); ok {
			remote.Close()
		}
	}
}

func (sup *Stackup) Debug(value bool) {
	sup.debug = value
}

func (sup *Stackup) Prefix(value bool) {
	sup.prefix = value
}
