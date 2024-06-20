package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/gchux/pcap-cli/pkg/pcap"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"github.com/lrita/cmap"
)

func UNUSED(x ...interface{}) {}

var (
	use_cron  = flag.Bool("use_cron", false, "perform packet capture at specific intervals")
	cron_exp  = flag.String("cron_exp", "", "stardard cron expression; i/e: '1 * * * *'")
	timezone  = flag.String("timezone", "UTC", "TimeZone to be used to schedule packet captures")
	duration  = flag.Int("timeout", 0, "perform packet capture during this mount of seconds")
	interval  = flag.Int("interval", 60, "seconds after which tcpdump rotates PCAP files")
	snaplen   = flag.Int("snaplen", 0, "bytes to be captured from each packet")
	filter    = flag.String("filter", "", "BPF filter to be used for capturing packets")
	extension = flag.String("extension", "pcap", "extension to be used for tcpdump PCAP files")
	directory = flag.String("directory", "", "directory where PCAP files will be stored")
	tcp_dump  = flag.Bool("tcpdump", true, "enable JSON PCAP using tcpdump")
	json_dump = flag.Bool("jsondump", false, "enable JSON PCAP using gopacket")
	json_log  = flag.Bool("jsonlog", false, "enable JSON PCAP to stardard output")
	ordered   = flag.Bool("ordered", false, "write JSON PCAP output as obtained from gopacket")
)

var (
	ifacePattern string = os.Getenv("PCAP_IFACE")
	sidecar      string = os.Getenv("APP_SIDECAR")
	module       string = os.Getenv("PROC_NAME")
)

var (
	jobs            cmap.Map[uuid.UUID, *tcpdumpJob]
	jid, xid        atomic.Value
	emptyTcpdumpJob = tcpdumpJob{Jid: uuid.Nil.String()}
)

type (
	pcapTask struct {
		engine  pcap.PcapEngine   `json:"-"`
		writers []pcap.PcapWriter `json:"-"`
	}

	tcpdumpJob struct {
		j     *gocron.Job     `json:"-"`
		Xid   string          `json:"xid,omitempty"`
		Jid   string          `json:"jid,omitempty"`
		Name  string          `json:"name,omitempty"`
		Tags  []string        `json:"-"`
		tasks []*pcapTask     `json:"-"`
		ctx   context.Context `json:"-"`
	}

	jLogLevel string

	jLogEntry struct {
		Severity jLogLevel  `json:"severity"`
		Message  string     `json:"message"`
		Sidecar  string     `json:"sidecar"`
		Module   string     `json:"module"`
		Job      tcpdumpJob `json:"job,omitempty"`
		Tags     []string   `json:"tags,omitempty"`
	}
)

const (
	INFO  jLogLevel = "INFO"
	ERROR jLogLevel = "ERROR"
	FATAL jLogLevel = "FATAL"
)

func jlog(severity jLogLevel, job *tcpdumpJob, message string) {
	j := *job
	j.Xid = xid.Load().(uuid.UUID).String()

	entry := &jLogEntry{
		Severity: severity,
		Message:  message,
		Sidecar:  sidecar,
		Module:   module,
		Job:      j,
		Tags:     j.Tags,
	}

	jEntry, err := json.Marshal(entry)
	if err != nil {
		fmt.Errorf("[ERROR] - %s\n", err)
		return
	}
	fmt.Println(string(jEntry))
}

func afterTcpdump(id uuid.UUID, name string) {
	if job, ok := jobs.Load(id); ok {
		jlog(INFO, job, "execution complete")
		j := *job.j
		nextRun, _ := j.NextRun()
		jlog(INFO, job, fmt.Sprintf("next execution: %v", nextRun))
	}
	xid.Store(uuid.Nil) // reset execution id
}

func beforeTcpdump(id uuid.UUID, name string) {
	if job, ok := jobs.Load(id); ok {
		j := *job.j
		lastRun, _ := j.LastRun()
		jlog(INFO, job, fmt.Sprintf("execution started ( last execution: %v )", lastRun))
	}
	xid.Store(uuid.New())
}

func start(ctx context.Context, timeout time.Duration, tasks []*pcapTask) error {
	// all PCAP engines are context aware
	var wg sync.WaitGroup
	wg.Add(len(tasks))

	var cancel context.CancelFunc
	if timeout > 0*time.Second {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for _, task := range tasks {
		go func(ctx context.Context, t *pcapTask) {
			defer wg.Done()
			t.engine.Start(ctx, t.writers)
		}(ctx, task)
	}

	// wait for context cancel/timeout
	<-ctx.Done()

	// wait for tasks to gracefully stop
	wg.Wait()

	return nil
}

func tcpdump(timeout time.Duration) error {
	jobID := jid.Load().(uuid.UUID)
	exeID := xid.Load().(uuid.UUID).String()

	var job *tcpdumpJob
	var ok bool
	if job, ok = jobs.Load(jobID); !ok {
		message := fmt.Sprintf("job[id:%s] not found", jobID)
		jlog(ERROR, &emptyTcpdumpJob, message)
		return fmt.Errorf(message)
	}

	// enable PCAP tasks with context awareness
	ctx := context.WithValue(job.ctx, "id", fmt.Sprintf("%s/%s", jobID.String(), exeID))

	return start(ctx, timeout, job.tasks)
}

func newPcapConfig(iface, format, output, extension, filter string, snaplen, interval int) *pcap.PcapConfig {
	return &pcap.PcapConfig{
		Promisc:   true,
		Iface:     iface,
		Snaplen:   snaplen,
		TsType:    "",
		Format:    format,
		Output:    output,
		Extension: extension,
		Filter:    filter,
		Interval:  interval,
	}
}

func createTasks(timezone, directory, extension, filter *string, snaplen, interval *int, tcpdump, jsondump, jsonlog, ordered *bool) []*pcapTask {
	tasks := []*pcapTask{}

	ifaceRegexp := regexp.MustCompile(fmt.Sprintf("^(?:ipvlan-)?%s\\d+.*", ifacePattern))
	devices, _ := pcap.FindDevicesByRegex(ifaceRegexp)

	for _, iface := range devices {

		netIface, err := net.InterfaceByName(iface)
		if err != nil {
			jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("invalid iface: %s (%s)", iface, err))
			continue
		}

		iface = netIface.Name
		ifaceAndIndex := fmt.Sprintf("%d/%s", netIface.Index, iface)

		jlog(INFO, &emptyTcpdumpJob, fmt.Sprintf("configuring PCAP for iface: %s", ifaceAndIndex))

		output := fmt.Sprintf("%s/part__%d_%s__%%Y%%m%%d_%%H%%M%%S", *directory, netIface.Index, netIface.Name)

		tcpdumpCfg := newPcapConfig(iface, "pcap", output, *extension, *filter, *snaplen, *interval)
		jsondumpCfg := newPcapConfig(iface, "json", output, "json", *filter, *snaplen, *interval)

		// premature optimization is the root of all evil
		var engineErr, writerErr error = nil, nil
		var tcpdumpEngine, jsondumpEngine pcap.PcapEngine = nil, nil
		var jsondumpWriter, jsonlogWriter pcap.PcapWriter = nil, nil // `tcpdump` does not use custom writers

		if *tcpdump {
			tcpdumpEngine, engineErr = pcap.NewTcpdump(tcpdumpCfg)
		} else {
			engineErr = fmt.Errorf("disabled")
		}

		if engineErr == nil {
			tasks = append(tasks, &pcapTask{engine: tcpdumpEngine, writers: nil})
			jlog(INFO, &emptyTcpdumpJob, fmt.Sprintf("configured 'tcpdump' for iface: %s", ifaceAndIndex))
		} else {
			jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("tcpdump task creation failed: %s (%s)", ifaceAndIndex, engineErr))
		}

		// skip JSON setup if no form fo JSON support is enabled
		if !*jsondump || !*jsonlog {
			continue
		}

		engineErr = nil

		jsondumpCfg.Ordered = *ordered

		// some form of JSON packet capturing is enabled
		jsondumpEngine, engineErr = pcap.NewPcap(jsondumpCfg)
		if engineErr != nil {
			jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("jsondump task creation failed: %s (%s)", ifaceAndIndex, engineErr))
			continue // abort all JSON setup for this device
		}

		pcapWriters := []pcap.PcapWriter{}

		// writing JSON PCAP file is only enabled if `jsondump` is enabled
		if *jsondump {
			jsondumpWriter, writerErr = pcap.NewPcapWriter(&output, &jsondumpCfg.Extension, timezone, *interval)
		}

		if writerErr != nil {
			jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("jsondump file writer creation failed: %s (%s)", ifaceAndIndex, writerErr))
		} else {
			pcapWriters = append(pcapWriters, jsondumpWriter)
		}

		if !*jsonlog && writerErr == nil {
			tasks = append(tasks, &pcapTask{jsondumpEngine, pcapWriters})
			continue
		}

		// add `/dev/stdout` as an additional PCAP writer
		jsonlogWriter, writerErr = pcap.NewStdoutPcapWriter()
		if writerErr != nil {
			jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("jsondump stdout writer creation failed: %s (%s)", ifaceAndIndex, writerErr))
			continue
		} else {
			pcapWriters = append(pcapWriters, jsonlogWriter)
		}

		jlog(INFO, &emptyTcpdumpJob, fmt.Sprintf("configured 'jsondump' for iface: %s", ifaceAndIndex))
		tasks = append(tasks, &pcapTask{jsondumpEngine, pcapWriters})
	}

	return tasks
}

func main() {
	flag.Parse()

	jid.Store(uuid.Nil)
	xid.Store(uuid.Nil)

	jlog(INFO, &emptyTcpdumpJob,
		fmt.Sprintf("args[use_cron:%t|cron_exp:%s|timezone:%s|timeout:%d|extension:%s|directory:%s|snaplen:%d|filter:%s|interval:%d|tcpdump:%t|jsondump:%t|jsonlog:%t|ordered:%t]",
			*use_cron, *cron_exp, *timezone, *duration, *extension, *directory, *snaplen, *filter, *interval, *tcp_dump, *json_dump, *json_log, *ordered))

	tasks := createTasks(timezone, directory, extension, filter, snaplen, interval, tcp_dump, json_dump, json_log, ordered)

	if len(tasks) == 0 {
		jlog(ERROR, &emptyTcpdumpJob, "no PCAP tasks available")
		os.Exit(1)
	}

	timeout := time.Duration(*duration) * time.Second
	jlog(INFO, &emptyTcpdumpJob, fmt.Sprintf("parsed timeout: %v", timeout))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Skip scheduling, execute `tcpdump`
	if !*use_cron {
		start(ctx, timeout, tasks)
		return
	}

	// The `timezone` to be used when scheduling `tcpdump` cron jobs
	location, err := time.LoadLocation(*timezone)
	if err != nil {
		*timezone = "UTC"
		location = time.UTC
		jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("could not load timezone '%s': %v", *timezone, err))
	}
	jlog(INFO, &emptyTcpdumpJob, fmt.Sprintf("parsed timezone: %v", location))

	// Create a scheduler using the requested timezone.
	// no more than 1 packet capturing job should ever be executed.
	s, err := gocron.NewScheduler(
		gocron.WithLimitConcurrentJobs(1, gocron.LimitModeReschedule),
		gocron.WithLocation(location),
		gocron.WithGlobalJobOptions(
			gocron.WithTags(
				os.Getenv("PROJECT_ID"),
				os.Getenv("APP_SERVICE"),
				os.Getenv("GCP_REGION"),
				os.Getenv("APP_REVISION"),
				os.Getenv("INSTANCE_ID"),
			),
		),
	)
	if err != nil {
		jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("failed to create scheduler: %v", err))
		os.Exit(2)
	}

	// Use the provided `cron` expression ro schedule the packet capturing job
	j, err := s.NewJob(
		gocron.CronJob(fmt.Sprintf("TZ=%s %s", *timezone, *cron_exp), true),
		gocron.NewTask(tcpdump, timeout),
		gocron.WithName("tcpdump"),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
		gocron.WithEventListeners(
			gocron.AfterJobRuns(afterTcpdump),
			gocron.BeforeJobRuns(beforeTcpdump),
		),
	)
	if err != nil {
		jlog(ERROR, &emptyTcpdumpJob, fmt.Sprintf("failed to create scheduled job: %v", err))
		s.Shutdown()
		os.Exit(3)
	}

	jid.Store(j.ID())

	job := &tcpdumpJob{
		ctx:   ctx,
		tasks: tasks,
		Jid:   j.ID().String(),
		Name:  j.Name(),
		Tags:  j.Tags(),
		j:     &j,
	}
	jobs.Store(j.ID(), job)
	jlog(INFO, job, "scheduled job")

	// Start the packet capturing scheduler
	s.Start()

	nextRun, _ := j.NextRun()
	jlog(INFO, job, fmt.Sprintf("next execution: %v", nextRun))

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	go func(job *tcpdumpJob) {
		signal := <-signals
		jlog(INFO, job, fmt.Sprintf("signaled: %v", signal))
		cancel()
	}(job)

	// Block main goroutine forever.
	<-ctx.Done()

	s.StopJobs()
	s.RemoveJob(j.ID())
	s.Shutdown()
}
