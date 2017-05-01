package cell_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"code.cloudfoundry.org/archiver/compressor"
	archive_helper "code.cloudfoundry.org/archiver/extractor/test_helper"
	"code.cloudfoundry.org/bbs/models"
	ssh_helpers "code.cloudfoundry.org/diego-ssh/helpers"
	"code.cloudfoundry.org/diego-ssh/routes"
	"code.cloudfoundry.org/inigo/fixtures"
	"code.cloudfoundry.org/inigo/helpers"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"
	"golang.org/x/crypto/ssh"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("SSH", func() {
	verifySSH := func(address, processGuid string, index int) {
		clientConfig := &ssh.ClientConfig{
			User: fmt.Sprintf("diego:%s/%d", processGuid, index),
			Auth: []ssh.AuthMethod{ssh.Password("")},
		}

		client, err := ssh.Dial("tcp", address, clientConfig)
		Expect(err).NotTo(HaveOccurred())

		session, err := client.NewSession()
		Expect(err).NotTo(HaveOccurred())

		output, err := session.Output("env")
		Expect(err).NotTo(HaveOccurred())

		Expect(string(output)).To(ContainSubstring("USER=root"))
		Expect(string(output)).To(ContainSubstring("TEST=foobar"))
		Expect(string(output)).To(ContainSubstring(fmt.Sprintf("INSTANCE_INDEX=%d", index)))
	}

	var (
		processGuid         string
		fileServerStaticDir string

		runtime ifrit.Process
		address string

		lrp models.DesiredLRP
	)

	BeforeEach(func() {
		processGuid = helpers.GenerateGuid()
		address = componentMaker.Addresses.SSHProxy

		var fileServer ifrit.Runner
		fileServer, fileServerStaticDir = componentMaker.FileServer()
		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServer},
			{"rep", componentMaker.Rep()},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
			{"ssh-proxy", componentMaker.SSHProxy()},
		}))

		tgCompressor := compressor.NewTgz()
		err := tgCompressor.Compress(componentMaker.Artifacts.Executables["sshd"], filepath.Join(fileServerStaticDir, "sshd.tgz"))
		Expect(err).NotTo(HaveOccurred())

		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			fixtures.GoServerApp(),
		)

		sshRoute := routes.SSHRoute{
			ContainerPort:   3456,
			PrivateKey:      componentMaker.SSHConfig.PrivateKeyPem,
			HostFingerprint: ssh_helpers.MD5Fingerprint(componentMaker.SSHConfig.HostKey.PublicKey()),
		}

		sshRoutePayload, err := json.Marshal(sshRoute)
		Expect(err).NotTo(HaveOccurred())

		sshRouteMessage := json.RawMessage(sshRoutePayload)

		envVars := []*models.EnvironmentVariable{
			{Name: "TEST", Value: "foobar"},
		}

		lrp = models.DesiredLRP{
			ProcessGuid:        processGuid,
			Domain:             "inigo",
			Instances:          2,
			Privileged:         true,
			LegacyDownloadUser: "vcap",
			Setup: models.WrapAction(models.Serial(
				&models.DownloadAction{
					Artifact: "sshd",
					From:     fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "sshd.tgz"),
					To:       "/tmp/diego",
					CacheKey: "sshd",
					User:     "root",
				},
				&models.DownloadAction{
					Artifact: "go-server",
					From:     fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
					To:       "/tmp/diego",
					CacheKey: "lrp-cache-key",
					User:     "root",
				},
			)),
			Action: models.WrapAction(models.Codependent(
				&models.RunAction{
					User: "root",
					Path: "/tmp/diego/sshd",
					Args: []string{
						"-address=0.0.0.0:3456",
						"-hostKey=" + componentMaker.SSHConfig.HostKeyPem,
						"-authorizedKey=" + componentMaker.SSHConfig.AuthorizedKey,
						"-inheritDaemonEnv",
					},
				},
				&models.RunAction{
					User: "root",
					Path: "/tmp/diego/go-server",
					Env:  []*models.EnvironmentVariable{{"PORT", "9999"}},
				},
			)),
			Monitor: models.WrapAction(&models.RunAction{
				User: "root",
				Path: "nc",
				Args: []string{"-z", "127.0.0.1", "3456"},
			}),
			StartTimeoutMs: 60000,
			RootFs:         "preloaded:" + helpers.PreloadedStacks[0],
			MemoryMb:       128,
			DiskMb:         128,
			Ports:          []uint32{3456},
			Routes: &models.Routes{
				routes.DIEGO_SSH: &sshRouteMessage,
			},
			EnvironmentVariables: envVars,
		}
	})

	JustBeforeEach(func() {
		logger := lagertest.NewTestLogger("test")
		logger.Info("desired-ssh-lrp", lager.Data{"lrp": lrp})

		err := bbsClient.DesireLRP(logger, &lrp)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() []*models.ActualLRPGroup {
			lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(logger, processGuid)
			Expect(err).NotTo(HaveOccurred())
			return lrps
		}).Should(HaveLen(2))

		Eventually(
			helpers.LRPInstanceStatePoller(logger, bbsClient, processGuid, 0, nil),
		).Should(Equal(models.ActualLRPStateRunning))

		Eventually(
			helpers.LRPInstanceStatePoller(logger, bbsClient, processGuid, 1, nil),
		).Should(Equal(models.ActualLRPStateRunning))
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime)
	})

	Context("when valid process guid and index are used in the username", func() {
		It("can ssh to appropriate app instance container", func() {
			verifySSH(address, processGuid, 0)
			verifySSH(address, processGuid, 1)
		})

		It("supports local port fowarding", func() {
			clientConfig := &ssh.ClientConfig{
				User: fmt.Sprintf("diego:%s/%d", processGuid, 0),
				Auth: []ssh.AuthMethod{ssh.Password("")},
			}

			client, err := ssh.Dial("tcp", address, clientConfig)
			Expect(err).NotTo(HaveOccurred())

			httpClient := &http.Client{
				Transport: &http.Transport{
					Dial: client.Dial,
				},
				Timeout: 20 * time.Second,
			}

			resp, err := httpClient.Get("http://localhost:9999/yo")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			contents, err := ioutil.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(contents).To(ContainSubstring("sup dawg"))
		})

		Context("when invalid password is used", func() {
			var clientConfig *ssh.ClientConfig

			BeforeEach(func() {
				clientConfig = &ssh.ClientConfig{
					User: "diego:" + processGuid + "/0",
					Auth: []ssh.AuthMethod{ssh.Password("invalid:password")},
				}
			})

			It("returns an error", func() {
				Eventually(
					helpers.LRPInstanceStatePoller(logger, bbsClient, processGuid, 0, nil),
				).Should(Equal(models.ActualLRPStateRunning))

				_, err := ssh.Dial("tcp", address, clientConfig)
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when a bare-bones docker image is used as the root filesystem", func() {
			BeforeEach(func() {
				lrp.StartTimeoutMs = 120000
				lrp.RootFs = "docker:///cloudfoundry/diego-docker-app"

				// busybox nc doesn't support -z
				lrp.Monitor = models.WrapAction(&models.RunAction{
					User: "root",
					Path: "sh",
					Args: []string{
						"-c",
						"echo -n '' | telnet localhost 3456 >/dev/null 2>&1 && echo -n '' | telnet localhost 9999 >/dev/null 2>&1 && true",
					},
				})
			})

			It("can ssh to appropriate app instance container", func() {
				verifySSH(address, processGuid, 0)
				verifySSH(address, processGuid, 1)
			})

			It("supports local port fowarding", func() {
				clientConfig := &ssh.ClientConfig{
					User: fmt.Sprintf("diego:%s/%d", processGuid, 0),
					Auth: []ssh.AuthMethod{ssh.Password("")},
				}

				client, err := ssh.Dial("tcp", address, clientConfig)
				Expect(err).NotTo(HaveOccurred())

				httpClient := &http.Client{
					Transport: &http.Transport{
						Dial: client.Dial,
					},
					Timeout: 20 * time.Second,
				}

				resp, err := httpClient.Get("http://localhost:9999/yo")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				contents, err := ioutil.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())
				Expect(contents).To(ContainSubstring("sup dawg"))
			})
		})
	})

	Context("when non-existent index is used as part of username", func() {
		var clientConfig *ssh.ClientConfig

		BeforeEach(func() {
			clientConfig = &ssh.ClientConfig{
				User: "diego:" + processGuid + "/3",
				Auth: []ssh.AuthMethod{ssh.Password("")},
			}
		})

		It("returns an error", func() {
			_, err := ssh.Dial("tcp", address, clientConfig)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("when non-existent process guid is used as part of username", func() {
		var clientConfig *ssh.ClientConfig

		BeforeEach(func() {
			clientConfig = &ssh.ClientConfig{
				User: "diego:not-existing-process-guid/0",
				Auth: []ssh.AuthMethod{ssh.Password("")},
			}
		})

		It("returns an error", func() {
			_, err := ssh.Dial("tcp", address, clientConfig)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("when invalid username format is used", func() {
		var clientConfig *ssh.ClientConfig

		BeforeEach(func() {
			clientConfig = &ssh.ClientConfig{
				User: "root",
				Auth: []ssh.AuthMethod{ssh.Password("some-password")},
			}
		})

		It("returns an error", func() {
			_, err := ssh.Dial("tcp", address, clientConfig)
			Expect(err).To(HaveOccurred())
		})
	})
})
