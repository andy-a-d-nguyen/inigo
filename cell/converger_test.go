package cell_test

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/cloudfoundry-incubator/inigo/fixtures"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	archive_helper "github.com/pivotal-golang/archiver/extractor/test_helper"
)

var _ = Describe("Convergence to desired state", func() {
	var (
		runtime ifrit.Process

		auctioneer ifrit.Process
		executor   ifrit.Process
		rep        ifrit.Process
		converger  ifrit.Process

		appId       string
		processGuid string

		runningLRPsPoller        func() []receptor.ActualLRPResponse
		helloWorldInstancePoller func() []string
	)

	BeforeEach(func() {
		fileServer, fileServerStaticDir := componentMaker.FileServer()

		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"file-server", fileServer},
			{"route-emitter", componentMaker.RouteEmitter()},
			{"router", componentMaker.Router()},
		}))

		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			fixtures.HelloWorldIndexLRP(),
		)

		appId = helpers.GenerateGuid()

		processGuid = helpers.GenerateGuid()

		runningLRPsPoller = func() []receptor.ActualLRPResponse {
			return helpers.ActiveActualLRPs(receptorClient, processGuid)
		}

		helloWorldInstancePoller = helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)
	})

	AfterEach(func() {
		By("Stopping all the processes")
		helpers.StopProcesses(auctioneer, executor, rep, converger, runtime)
	})

	Describe("Executor fault tolerance", func() {
		BeforeEach(func() {
			auctioneer = ginkgomon.Invoke(componentMaker.Auctioneer())
		})

		Context("when an executor, rep, and converger are running", func() {
			BeforeEach(func() {
				executor = ginkgomon.Invoke(componentMaker.Executor())
				rep = ginkgomon.Invoke(componentMaker.Rep())
				converger = ginkgomon.Invoke(componentMaker.Converger(
					"-convergeRepeatInterval", "1s",
				))
			})

			Context("and an LRP is desired", func() {
				BeforeEach(func() {
					err := receptorClient.CreateDesiredLRP(helpers.DefaultLRPCreateRequest(processGuid, appId, 2))
					Ω(err).ShouldNot(HaveOccurred())

					Eventually(runningLRPsPoller).Should(HaveLen(2))
					Eventually(helloWorldInstancePoller).Should(Equal([]string{"0", "1"}))
				})

				Context("and the LRP goes away because its executor dies", func() {
					BeforeEach(func() {
						executor.Signal(syscall.SIGKILL)

						Eventually(runningLRPsPoller).Should(BeEmpty())
						Eventually(helloWorldInstancePoller).Should(BeEmpty())
					})

					Context("once the executor comes back", func() {
						BeforeEach(func() {
							executor = ginkgomon.Invoke(componentMaker.Executor())
						})

						It("eventually brings the long-running process up", func() {
							Eventually(runningLRPsPoller).Should(HaveLen(2))
							Eventually(helloWorldInstancePoller).Should(Equal([]string{"0", "1"}))
						})
					})
				})

				Context("and a new rep is introduced", func() {
					var firstActualLRPs []receptor.ActualLRPResponse
					var rep2 ifrit.Process

					BeforeEach(func() {
						firstActualLRPs = runningLRPsPoller()
						rep2 = ginkgomon.Invoke(componentMaker.RepN(1))
					})

					AfterEach(func() {
						helpers.StopProcesses(rep2)
					})

					Context("and the first rep goes away", func() {
						BeforeEach(func() {
							rep.Signal(syscall.SIGKILL)
						})

						It("eventually brings up the LRP on the new rep", func() {
							Eventually(func() bool {
								secondActualLRPs := runningLRPsPoller()
								if len(secondActualLRPs) != 2 {
									return false
								}
								return secondActualLRPs[0].CellID != firstActualLRPs[0].CellID &&
									secondActualLRPs[1].CellID != firstActualLRPs[1].CellID
							}).Should(BeTrue())
						})
					})
				})

				Context("and the rep and converger go away", func() {
					BeforeEach(func() {
						converger.Signal(syscall.SIGKILL)
						rep.Signal(syscall.SIGKILL)
					})

					Context("and the LRP is scaled down (but the event is not handled)", func() {
						BeforeEach(func() {
							onePlease := 1

							err := receptorClient.UpdateDesiredLRP(processGuid, receptor.DesiredLRPUpdateRequest{
								Instances: &onePlease,
							})
							Ω(err).ShouldNot(HaveOccurred())

							Consistently(runningLRPsPoller).Should(HaveLen(2))
						})

						Context("and rep and converger come back", func() {
							BeforeEach(func() {
								rep = ginkgomon.Invoke(componentMaker.Rep())
								converger = ginkgomon.Invoke(componentMaker.Converger(
									"-convergeRepeatInterval", "1s",
								))
							})

							It("eventually scales the LRP down", func() {
								Eventually(runningLRPsPoller).Should(HaveLen(1))
								Eventually(helloWorldInstancePoller).Should(Equal([]string{"0"}))
							})
						})
					})
				})
			})
		})

		Context("when a converger is running without a rep and executor", func() {
			BeforeEach(func() {
				converger = ginkgomon.Invoke(componentMaker.Converger(
					"-convergeRepeatInterval", "1s",
				))
			})

			Context("and an LRP is desired", func() {
				BeforeEach(func() {
					err := receptorClient.CreateDesiredLRP(helpers.DefaultLRPCreateRequest(processGuid, appId, 1))
					Ω(err).ShouldNot(HaveOccurred())

					Consistently(runningLRPsPoller).Should(BeEmpty())
					Consistently(helloWorldInstancePoller).Should(BeEmpty())
				})

				Context("and then a rep and executor come up", func() {
					BeforeEach(func() {
						executor = ginkgomon.Invoke(componentMaker.Executor())
						rep = ginkgomon.Invoke(componentMaker.Rep())
					})

					It("eventually brings the LRP up", func() {
						Eventually(runningLRPsPoller).Should(HaveLen(1))
						Eventually(helloWorldInstancePoller).Should(Equal([]string{"0"}))
					})
				})
			})
		})
	})

	Describe("Auctioneer Fault Tolerance", func() {
		BeforeEach(func() {
			converger = ginkgomon.Invoke(componentMaker.Converger(
				"-convergeRepeatInterval", "1s",
			))
		})

		Context("when an executor and rep are running with no auctioneer", func() {
			BeforeEach(func() {
				executor = ginkgomon.Invoke(componentMaker.Executor())
				rep = ginkgomon.Invoke(componentMaker.Rep())
			})

			Context("and an LRP is desired", func() {
				BeforeEach(func() {
					err := receptorClient.CreateDesiredLRP(helpers.DefaultLRPCreateRequest(processGuid, appId, 1))
					Ω(err).ShouldNot(HaveOccurred())

					Consistently(runningLRPsPoller).Should(BeEmpty())
					Consistently(helloWorldInstancePoller).Should(BeEmpty())
				})

				Context("and then an auctioneer comes up", func() {
					BeforeEach(func() {
						auctioneer = ginkgomon.Invoke(componentMaker.Auctioneer())
					})

					It("eventually brings it up", func() {
						Eventually(runningLRPsPoller).Should(HaveLen(1))
						Eventually(helloWorldInstancePoller).Should(Equal([]string{"0"}))
					})
				})
			})
		})

		Context("when an auctioneer is running with no executor or rep", func() {
			BeforeEach(func() {
				auctioneer = ginkgomon.Invoke(componentMaker.Auctioneer())
			})

			Context("and an LRP is desired", func() {
				BeforeEach(func() {
					err := receptorClient.CreateDesiredLRP(helpers.DefaultLRPCreateRequest(processGuid, appId, 1))
					Ω(err).ShouldNot(HaveOccurred())

					Consistently(runningLRPsPoller).Should(BeEmpty())
					Consistently(helloWorldInstancePoller).Should(BeEmpty())
				})

				Context("and the executor and rep come up", func() {
					BeforeEach(func() {
						executor = ginkgomon.Invoke(componentMaker.Executor())
						rep = ginkgomon.Invoke(componentMaker.Rep())
					})

					It("eventually brings it up", func() {
						Eventually(runningLRPsPoller).Should(HaveLen(1))
						Eventually(helloWorldInstancePoller).Should(Equal([]string{"0"}))
					})
				})
			})
		})
	})
})
