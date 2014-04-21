package inigo_test

import (
	"archive/tar"
	"compress/gzip"
	"time"

	"github.com/cloudfoundry/gunk/timeprovider"

	"github.com/cloudfoundry-incubator/inigo/executor_runner"
	"github.com/cloudfoundry-incubator/inigo/inigo_server"
	"github.com/cloudfoundry-incubator/inigo/loggredile"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry-incubator/runtime-schema/models/factories"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/vito/cmdtest/matchers"
)

var _ = Describe("Executor", func() {
	var bbs *Bbs.BBS

	BeforeEach(func() {
		bbs = Bbs.New(suiteContext.EtcdRunner.Adapter(), timeprovider.NewTimeProvider())
	})

	Describe("when starting with invalid memory/disk", func() {
		It("should exit with failure", func() {
			suiteContext.ExecutorRunner.StartWithoutCheck(executor_runner.Config{MemoryMB: -1, DiskMB: -1})
			Ω(suiteContext.ExecutorRunner.Session).Should(SayWithTimeout("valid memory and disk capacity must be specified", time.Second))
			Ω(suiteContext.ExecutorRunner.Session).Should(ExitWith(1))
		})
	})

	Describe("Heartbeating", func() {
		It("should heartbeat its presence", func() {
			suiteContext.ExecutorRunner.Start()

			Eventually(func() interface{} {
				executors, _ := bbs.GetAllExecutors()
				return executors
			}).Should(HaveLen(1))
		})
	})

	Describe("Resource limits", func() {
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()
		})

		It("should only pick up tasks if it has capacity", func() {
			firstGuyGuid := factories.GenerateGuid()
			secondGuyGuid := factories.GenerateGuid()
			firstGuyTask := factories.BuildTaskWithRunAction(suiteContext.ExecutorRunner.Config.Stack, 1024, 1024, inigo_server.CurlCommand(firstGuyGuid)+"; sleep 5")
			bbs.DesireTask(firstGuyTask)

			Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(firstGuyGuid))

			secondGuyTask := factories.BuildTaskWithRunAction(suiteContext.ExecutorRunner.Config.Stack, 1024, 1024, inigo_server.CurlCommand(secondGuyGuid))
			bbs.DesireTask(secondGuyTask)

			Consistently(inigo_server.ReportingGuids, SHORT_TIMEOUT).ShouldNot(ContainElement(secondGuyGuid))
		})
	})

	Describe("Stack", func() {
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start(executor_runner.Config{Stack: "penguin"})
		})

		It("should only pick up tasks if the stacks match", func() {
			matchingGuid := factories.GenerateGuid()
			matchingTask := factories.BuildTaskWithRunAction(suiteContext.ExecutorRunner.Config.Stack, 100, 100, inigo_server.CurlCommand(matchingGuid)+"; sleep 10")
			matchingTask.Stack = "penguin"

			nonMatchingGuid := factories.GenerateGuid()
			nonMatchingTask := factories.BuildTaskWithRunAction(suiteContext.ExecutorRunner.Config.Stack, 100, 100, inigo_server.CurlCommand(nonMatchingGuid)+"; sleep 10")
			nonMatchingTask.Stack = "lion"

			bbs.DesireTask(matchingTask)
			bbs.DesireTask(nonMatchingTask)

			Consistently(inigo_server.ReportingGuids, SHORT_TIMEOUT).ShouldNot(ContainElement(nonMatchingGuid), "Did not expect to see this app running, as it has the wrong stack.")
			Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(matchingGuid))
		})
	})

	Describe("Running a command", func() {
		var guid string
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()
			guid = factories.GenerateGuid()
		})

		It("should run the command with the provided environment", func() {
			env := []models.EnvironmentVariable{
				{"FOO", "BAR"},
				{"BAZ", "WIBBLE"},
				{"FOO", "$FOO-$BAZ"},
			}
			task := &models.Task{
				Guid:     factories.GenerateGuid(),
				Stack:    suiteContext.ExecutorRunner.Config.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Actions: []models.ExecutorAction{
					{Action: models.RunAction{Script: `test $FOO = "BAR-WIBBLE"`, Env: env}},
				},
			}

			bbs.DesireTask(task)

			Eventually(bbs.GetAllCompletedTasks, LONG_TIMEOUT).Should(HaveLen(1))

			tasks, _ := bbs.GetAllCompletedTasks()
			Ω(tasks[0].FailureReason).Should(BeEmpty())
			Ω(tasks[0].Failed).Should(BeFalse())
		})

		Context("when the command exceeds its memory limit", func() {
			var otherGuid string

			It("should fail the Task", func() {
				otherGuid = factories.GenerateGuid()
				task := &models.Task{
					Guid:     factories.GenerateGuid(),
					Stack:    suiteContext.ExecutorRunner.Config.Stack,
					MemoryMB: 10,
					DiskMB:   1024,
					Actions: []models.ExecutorAction{
						{Action: models.RunAction{Script: inigo_server.CurlCommand(guid)}},
						{Action: models.RunAction{Script: `ruby -e 'arr = "m"*1024*1024*100'`}},
						{Action: models.RunAction{Script: inigo_server.CurlCommand(otherGuid)}},
					},
				}

				bbs.DesireTask(task)

				Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(guid))

				Eventually(bbs.GetAllCompletedTasks, LONG_TIMEOUT).Should(HaveLen(1))
				tasks, _ := bbs.GetAllCompletedTasks()
				Ω(tasks[0].Failed).Should(BeTrue())
				Ω(tasks[0].FailureReason).Should(ContainSubstring("out of memory"))

				Ω(inigo_server.ReportingGuids()).ShouldNot(ContainElement(otherGuid))
			})
		})

		Context("when the command exceeds its file descriptor limit", func() {
			It("should fail the Task", func() {
				task := &models.Task{
					Guid:            factories.GenerateGuid(),
					Stack:           suiteContext.ExecutorRunner.Config.Stack,
					MemoryMB:        10,
					DiskMB:          1024,
					FileDescriptors: 1,
					Actions: []models.ExecutorAction{
						{Action: models.RunAction{Script: `ruby -e '10.times.each { |x| File.open("#{x}","w") }'`}},
					},
				}

				bbs.DesireTask(task)

				Eventually(bbs.GetAllCompletedTasks, LONG_TIMEOUT).Should(HaveLen(1))
				tasks, _ := bbs.GetAllCompletedTasks()
				Ω(tasks[0].Failed).Should(BeTrue())
				Ω(tasks[0].FailureReason).Should(ContainSubstring("127"))
			})
		})

		Context("when the command times out", func() {
			It("should fail the Task", func() {
				task := &models.Task{
					Guid:     factories.GenerateGuid(),
					Stack:    suiteContext.ExecutorRunner.Config.Stack,
					MemoryMB: 1024,
					DiskMB:   1024,
					Actions: []models.ExecutorAction{
						{Action: models.RunAction{Script: inigo_server.CurlCommand(guid)}},
						{Action: models.RunAction{Script: `sleep 0.8`, Timeout: 500 * time.Millisecond}},
					},
				}

				bbs.DesireTask(task)

				Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(guid))
				Eventually(bbs.GetAllCompletedTasks, LONG_TIMEOUT).Should(HaveLen(1))
				tasks, _ := bbs.GetAllCompletedTasks()
				Ω(tasks[0].Failed).Should(BeTrue())
				Ω(tasks[0].FailureReason).Should(ContainSubstring("Timed out after 500ms"))
			})
		})
	})

	Describe("Running a downloaded file", func() {
		var guid string
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()

			guid = factories.GenerateGuid()
			inigo_server.UploadFileString("curling.sh", inigo_server.CurlCommand(guid))
		})

		It("downloads the file", func() {
			task := &models.Task{
				Guid:     factories.GenerateGuid(),
				Stack:    suiteContext.ExecutorRunner.Config.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Actions: []models.ExecutorAction{
					{Action: models.DownloadAction{From: inigo_server.DownloadUrl("curling.sh"), To: "curling.sh", Extract: false}},
					{Action: models.RunAction{Script: "bash curling.sh"}},
				},
			}

			bbs.DesireTask(task)

			Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(guid))
		})
	})

	Describe("Uploading from the container", func() {
		var guid string
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()

			guid = factories.GenerateGuid()
		})

		It("uploads a tarball containing the specified files", func() {
			task := &models.Task{
				Guid:     factories.GenerateGuid(),
				Stack:    suiteContext.ExecutorRunner.Config.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Actions: []models.ExecutorAction{
					{Action: models.RunAction{Script: `echo "tasty thingy" > thingy`}},
					{Action: models.UploadAction{From: "thingy", To: inigo_server.UploadUrl("thingy")}},
					{Action: models.RunAction{Script: inigo_server.CurlCommand(guid)}},
				},
			}

			bbs.DesireTask(task)

			Eventually(inigo_server.ReportingGuids, LONG_TIMEOUT).Should(ContainElement(guid))

			downloadStream := inigo_server.DownloadFile("thingy")

			gw, err := gzip.NewReader(downloadStream)
			Ω(err).ShouldNot(HaveOccurred())

			tw := tar.NewReader(gw)

			_, err = tw.Next()
			Ω(err).ShouldNot(HaveOccurred())
		})
	})

	Describe("Fetching results", func() {
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()
		})

		It("should fetch the contents of the requested file and provide the content in the completed Task", func() {
			task := &models.Task{
				Guid:     factories.GenerateGuid(),
				Stack:    suiteContext.ExecutorRunner.Config.Stack,
				MemoryMB: 1024,
				DiskMB:   1024,
				Actions: []models.ExecutorAction{
					{Action: models.RunAction{Script: `echo "tasty thingy" > thingy`}},
					{Action: models.FetchResultAction{File: "thingy"}},
				},
			}

			bbs.DesireTask(task)

			Eventually(bbs.GetAllCompletedTasks, LONG_TIMEOUT).Should(HaveLen(1))

			tasks, _ := bbs.GetAllCompletedTasks()
			Ω(tasks[0].Result).Should(Equal("tasty thingy\n"))
		})
	})

	Describe("A Task with logging configured", func() {
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()
		})

		It("has its stdout and stderr emitted to Loggregator", func(done Done) {
			logGuid := factories.GenerateGuid()

			messages, stop := loggredile.StreamMessages(
				suiteContext.LoggregatorRunner.Config.OutgoingPort,
				"/tail/?app="+logGuid,
			)

			task := factories.BuildTaskWithRunAction(
				suiteContext.ExecutorRunner.Config.Stack,
				1024,
				1024,
				"echo out A; echo out B; echo out C; echo err A 1>&2; echo err B 1>&2; echo err C 1>&2",
			)
			task.Log.Guid = logGuid
			task.Log.SourceName = "APP"

			bbs.DesireTask(task)

			outStream := []string{}
			errStream := []string{}

			for i := 0; i < 6; i++ {
				message := <-messages
				switch message.GetMessageType() {
				case logmessage.LogMessage_OUT:
					outStream = append(outStream, string(message.GetMessage()))
				case logmessage.LogMessage_ERR:
					errStream = append(errStream, string(message.GetMessage()))
				}
			}

			Ω(outStream).Should(Equal([]string{"out A", "out B", "out C"}))
			Ω(errStream).Should(Equal([]string{"err A", "err B", "err C"}))

			close(stop)
			close(done)
		}, LONG_TIMEOUT)
	})
})
