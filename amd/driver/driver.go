package driver

import (
	"log"
	"reflect"
	"runtime/debug"
	"sync"

	"github.com/rs/xid"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
	"github.com/sarchlab/mgpusim/v4/amd/driver/internal"
	"github.com/sarchlab/mgpusim/v4/amd/kernels"
	"github.com/sarchlab/mgpusim/v4/amd/protocol"
	"github.com/tebeka/atexit"
)

// Driver is an Akita component that controls the simulated GPUs
type Driver struct {
	*sim.TickingComponent

	memAllocator  internal.MemoryAllocator
	distributor   distributor
	globalStorage *mem.Storage

	GPUs        []sim.Port
	devices     []*internal.Device
	pageTable   vm.PageTable
	middlewares []Middleware

	requestsToSend []sim.Msg

	contextMutex sync.Mutex
	contexts     []*Context

	mmuPort sim.Port
	gpuPort sim.Port

	driverStopped      chan bool
	enqueueSignal      chan bool
	engineMutex        sync.Mutex
	engineRunning      bool
	engineRunningMutex sync.Mutex
	simulationID       string

	Log2PageSize uint64

	currentPageMigrationReq         *vm.PageMigrationReqToDriver
	toSendToMMU                     *vm.PageMigrationRspFromDriver
	migrationReqToSendToCP          []*protocol.PageMigrationReqToCP
	isCurrentlyHandlingMigrationReq bool
	numRDMADrainACK                 uint64
	numRDMARestartACK               uint64
	numShootDownACK                 uint64
	numRestartACK                   uint64
	numPagesMigratingACK            uint64
	isCurrentlyMigratingOnePage     bool

	RemotePMCPorts []sim.Port
}

// Run starts a new threads that handles all commands in the command queues
func (d *Driver) Run() {
	d.logSimulationStart()
	go d.runAsync()
}

// Terminate stops the driver thread execution.
func (d *Driver) Terminate() {
	d.driverStopped <- true
	d.logSimulationTerminate()
}

func (d *Driver) logSimulationStart() {
	d.simulationID = xid.New().String()
	tracing.StartTask(
		d.simulationID,
		"",
		d,
		"Simulation", "Simulation",
		nil,
	)
}

func (d *Driver) logSimulationTerminate() {
	tracing.EndTask(d.simulationID, d)
}

func (d *Driver) runAsync() {
	for {
		select {
		case <-d.driverStopped:
			return
		case <-d.enqueueSignal:
			d.Engine.Pause()
			d.TickLater()
			d.Engine.Continue()

			d.engineRunningMutex.Lock()
			if d.engineRunning {
				d.engineRunningMutex.Unlock()
				continue
			}

			d.engineRunning = true
			go d.runEngine()
			d.engineRunningMutex.Unlock()
		}
	}
}

func (d *Driver) runEngine() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic: %v", r)
			debug.PrintStack()
			atexit.Exit(1)
		}
	}()

	d.engineMutex.Lock()
	defer d.engineMutex.Unlock()
	err := d.Engine.Run()
	if err != nil {
		panic(err)
	}

	d.engineRunningMutex.Lock()
	d.engineRunning = false
	d.engineRunningMutex.Unlock()
}

// DeviceProperties defines the properties of a device
type DeviceProperties struct {
	CUCount  int
	DRAMSize uint64
}

// RegisterGPU tells the driver about the existence of a GPU
func (d *Driver) RegisterGPU(
	commandProcessorPort sim.Port,
	properties DeviceProperties,
) {
	d.GPUs = append(d.GPUs, commandProcessorPort)

	gpuDevice := &internal.Device{
		ID:       len(d.GPUs),
		Type:     internal.DeviceTypeGPU,
		MemState: internal.NewDeviceMemoryState(d.Log2PageSize),
		Properties: internal.DeviceProperties{
			CUCount:  properties.CUCount,
			DRAMSize: properties.DRAMSize,
		},
	}
	gpuDevice.SetTotalMemSize(properties.DRAMSize)
	d.memAllocator.RegisterDevice(gpuDevice)

	d.devices = append(d.devices, gpuDevice)
}

// Tick ticks
func (d *Driver) Tick() bool {
	madeProgress := false

	madeProgress = d.sendToGPUs() || madeProgress
	madeProgress = d.sendToMMU() || madeProgress
	madeProgress = d.sendMigrationReqToCP() || madeProgress

	for _, mw := range d.middlewares {
		madeProgress = mw.Tick() || madeProgress
	}

	madeProgress = d.processReturnReq() || madeProgress
	madeProgress = d.processNewCommand() || madeProgress
	madeProgress = d.parseFromMMU() || madeProgress

	return madeProgress
}

func (d *Driver) sendToGPUs() bool {
	if len(d.requestsToSend) == 0 {
		return false
	}

	req := d.requestsToSend[0]
	err := d.gpuPort.Send(req)
	if err == nil {
		d.requestsToSend = d.requestsToSend[1:]
		return true
	}

	return false
}

//nolint:gocyclo
func (d *Driver) processReturnReq() bool {
	req := d.gpuPort.PeekIncoming()
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *protocol.LaunchKernelRsp:
		d.gpuPort.RetrieveIncoming()
		return d.processLaunchKernelReturn(req)
	case *protocol.RDMADrainRspToDriver:
		d.gpuPort.RetrieveIncoming()
		return d.processRDMADrainRsp(req)
	case *protocol.ShootDownCompleteRsp:
		d.gpuPort.RetrieveIncoming()
		return d.processShootdownCompleteRsp(req)
	case *protocol.PageMigrationRspToDriver:
		d.gpuPort.RetrieveIncoming()
		return d.processPageMigrationRspFromCP(req)
	case *protocol.RDMARestartRspToDriver:
		d.gpuPort.RetrieveIncoming()
		return d.processRDMARestartRspToDriver(req)
	case *protocol.GPURestartRsp:
		d.gpuPort.RetrieveIncoming()
		return d.handleGPURestartRsp(req)
	}

	return false
}

func (d *Driver) processNewCommand() bool {
	madeProgress := false

	d.contextMutex.Lock()
	for _, ctx := range d.contexts {
		madeProgress = d.processNewCommandFromContext(ctx) || madeProgress
	}
	d.contextMutex.Unlock()

	return madeProgress
}

func (d *Driver) processNewCommandFromContext(
	ctx *Context,
) bool {
	madeProgress := false
	ctx.queueMutex.Lock()
	for _, q := range ctx.queues {
		madeProgress = d.processNewCommandFromCmdQueue(q) || madeProgress
	}
	ctx.queueMutex.Unlock()

	return madeProgress
}

func (d *Driver) processNewCommandFromCmdQueue(
	q *CommandQueue,
) bool {
	if q.NumCommand() == 0 {
		return false
	}

	if q.IsRunning {
		return false
	}

	return d.processOneCommand(q)
}

func (d *Driver) processOneCommand(
	cmdQueue *CommandQueue,
) bool {
	cmd := cmdQueue.Peek()

	switch cmd := cmd.(type) {
	case *LaunchKernelCommand:
		d.logCmdStart(cmd)
		return d.processLaunchKernelCommand(cmd, cmdQueue)
	case *NoopCommand:
		d.logCmdStart(cmd)
		return d.processNoopCommand(cmd, cmdQueue)
	case *LaunchUnifiedMultiGPUKernelCommand:
		d.logCmdStart(cmd)
		return d.processUnifiedMultiGPULaunchKernelCommand(cmd, cmdQueue)
	default:
		return d.processCommandWithMiddleware(cmd, cmdQueue)
	}
}

func (d *Driver) processCommandWithMiddleware(
	cmd Command,
	cmdQueue *CommandQueue,
) bool {
	for _, m := range d.middlewares {
		processed := m.ProcessCommand(cmd, cmdQueue)

		if processed {
			d.logCmdStart(cmd)
			return true
		}
	}

	return false
}

func (d *Driver) logCmdStart(cmd Command) {
	tracing.StartTask(
		cmd.GetID(),
		d.simulationID,
		d,
		"Driver Command",
		reflect.TypeOf(cmd).String(),
		nil,
	)
}

func (d *Driver) logCmdComplete(cmd Command) {
	tracing.EndTask(cmd.GetID(), d)
}

func (d *Driver) processNoopCommand(
	cmd *NoopCommand,
	queue *CommandQueue,
) bool {
	queue.Dequeue()
	return true
}

func (d *Driver) logTaskToGPUInitiate(
	cmd Command,
	req sim.Msg,
) {
	tracing.TraceReqInitiate(req, d, cmd.GetID())
}

func (d *Driver) logTaskToGPUClear(
	req sim.Msg,
) {
	tracing.TraceReqFinalize(req, d)
}

func (d *Driver) processLaunchKernelCommand(
	cmd *LaunchKernelCommand,
	queue *CommandQueue,
) bool {
	req := protocol.NewLaunchKernelReq(d.gpuPort,
		d.GPUs[queue.GPUID-1])
	req.PID = queue.Context.pid
	req.HsaCo = cmd.CodeObject

	req.Packet = cmd.Packet
	req.PacketAddress = uint64(cmd.DPacket)

	queue.IsRunning = true
	cmd.Reqs = append(cmd.Reqs, req)

	d.requestsToSend = append(d.requestsToSend, req)

	queue.Context.l2Dirty = true
	queue.Context.markAllBuffersDirty()

	d.logTaskToGPUInitiate(cmd, req)

	return true
}

func (d *Driver) processUnifiedMultiGPULaunchKernelCommand(
	cmd *LaunchUnifiedMultiGPUKernelCommand,
	queue *CommandQueue,
) bool {
	wgDist := d.distributeWGToGPUs(queue, cmd)

	dev := d.devices[queue.GPUID]
	for i, gpuID := range dev.UnifiedGPUIDs {
		if wgDist[i+1]-wgDist[i] == 0 {
			continue
		}

		req := protocol.NewLaunchKernelReq(d.gpuPort, d.GPUs[gpuID-1])
		req.PID = queue.Context.pid
		req.HsaCo = cmd.CodeObject
		req.Packet = cmd.PacketArray[i]
		req.PacketAddress = uint64(cmd.DPacketArray[i])

		currentGPUIndex := i
		req.WGFilter = func(
			pkt *kernels.HsaKernelDispatchPacket,
			wg *kernels.WorkGroup,
		) bool {
			numWGX := (pkt.GridSizeX-1)/uint32(pkt.WorkgroupSizeX) + 1
			numWGY := (pkt.GridSizeY-1)/uint32(pkt.WorkgroupSizeY) + 1

			flattenedID :=
				wg.IDZ*int(numWGX)*int(numWGY) +
					wg.IDY*int(numWGX) +
					wg.IDX

			if flattenedID >= wgDist[currentGPUIndex] &&
				flattenedID < wgDist[currentGPUIndex+1] {
				return true
			}

			return false
		}

		queue.IsRunning = true
		cmd.Reqs = append(cmd.Reqs, req)

		d.requestsToSend = append(d.requestsToSend, req)

		queue.Context.l2Dirty = true
		queue.Context.markAllBuffersDirty()

		d.logTaskToGPUInitiate(cmd, req)
	}

	return true
}

func (d *Driver) distributeWGToGPUs(
	queue *CommandQueue,
	cmd *LaunchUnifiedMultiGPUKernelCommand,
) []int {
	dev := d.devices[queue.GPUID]
	actualGPUs := dev.UnifiedGPUIDs
	wgAllocated := 0
	wgDist := make([]int, len(actualGPUs)+1)

	totalCUCount := 0
	for _, devID := range actualGPUs {
		totalCUCount += d.devices[devID].Properties.CUCount
	}

	numWGX := (cmd.PacketArray[0].GridSizeX-1)/uint32(cmd.PacketArray[0].WorkgroupSizeX) + 1
	numWGY := (cmd.PacketArray[0].GridSizeY-1)/uint32(cmd.PacketArray[0].WorkgroupSizeY) + 1
	numWGZ := (cmd.PacketArray[0].GridSizeZ-1)/uint32(cmd.PacketArray[0].WorkgroupSizeZ) + 1
	totalWGCount := int(numWGX * numWGY * numWGZ)
	wgPerCU := (totalWGCount-1)/totalCUCount + 1

	for i, devID := range actualGPUs {
		cuCount := d.devices[devID].Properties.CUCount
		wgToAllocate := cuCount * wgPerCU
		wgDist[i+1] = wgAllocated + wgToAllocate
		wgAllocated += wgToAllocate
	}

	if wgAllocated < totalWGCount {
		panic("not all wg allocated")
	}

	return wgDist
}

func (d *Driver) processLaunchKernelReturn(
	rsp *protocol.LaunchKernelRsp,
) bool {
	req, cmd, cmdQueue := d.findCommandByReqID(rsp.RspTo)
	cmd.RemoveReq(req)

	d.logTaskToGPUClear(req)

	if len(cmd.GetReqs()) == 0 {
		cmdQueue.IsRunning = false
		cmdQueue.Dequeue()

		d.logCmdComplete(cmd)
	}

	return true
}

func (d *Driver) findCommandByReq(req sim.Msg) (Command, *CommandQueue) {
	d.contextMutex.Lock()
	defer d.contextMutex.Unlock()

	for _, ctx := range d.contexts {
		ctx.queueMutex.Lock()
		for _, q := range ctx.queues {
			cmd := q.Peek()
			if cmd == nil {
				continue
			}

			reqs := cmd.GetReqs()
			for _, r := range reqs {
				if r == req {
					ctx.queueMutex.Unlock()
					return cmd, q
				}
			}
		}
		ctx.queueMutex.Unlock()
	}

	panic("cannot find command")
}

func (d *Driver) findCommandByReqID(reqID string) (
	sim.Msg,
	Command,
	*CommandQueue,
) {
	d.contextMutex.Lock()
	defer d.contextMutex.Unlock()

	for _, ctx := range d.contexts {
		ctx.queueMutex.Lock()

		for _, q := range ctx.queues {
			cmd := q.Peek()
			if cmd == nil {
				continue
			}

			reqs := cmd.GetReqs()
			for _, r := range reqs {
				if r.Meta().ID == reqID {
					ctx.queueMutex.Unlock()
					return r, cmd, q
				}
			}
		}

		ctx.queueMutex.Unlock()
	}

	panic("cannot find command")
}

func (d *Driver) parseFromMMU() bool {
	if d.isCurrentlyHandlingMigrationReq {
		return false
	}

	req := d.mmuPort.RetrieveIncoming()
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *vm.PageMigrationReqToDriver:
		d.currentPageMigrationReq = req
		d.isCurrentlyHandlingMigrationReq = true
		d.initiateRDMADrain()
	default:
		log.Panicf("Driver cannot handle request of type %s",
			reflect.TypeOf(req))
	}

	return true
}

func (d *Driver) initiateRDMADrain() bool {
	for i := 0; i < len(d.GPUs); i++ {
		req := protocol.NewRDMADrainCmdFromDriver(d.gpuPort,
			d.GPUs[i])
		d.requestsToSend = append(d.requestsToSend, req)
		d.numRDMADrainACK++
	}

	return true
}

func (d *Driver) processRDMADrainRsp(
	req *protocol.RDMADrainRspToDriver,
) bool {
	d.numRDMADrainACK--

	if d.numRDMADrainACK == 0 {
		d.sendShootDownReqs()
	}

	return true
}

func (d *Driver) sendShootDownReqs() bool {
	vAddr := make([]uint64, 0)
	migrationInfo := d.currentPageMigrationReq.MigrationInfo

	numReqsGPUInMap := 0
	for i := 1; i < d.GetNumGPUs()+1; i++ {
		pages, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]

		if found {
			numReqsGPUInMap++
			for j := 0; j < len(pages); j++ {
				vAddr = append(vAddr, pages[j])
			}
		}
	}

	accessingGPUs := d.currentPageMigrationReq.CurrAccessingGPUs
	pid := d.currentPageMigrationReq.PID
	d.numShootDownACK = uint64(len(accessingGPUs))

	for i := 0; i < len(accessingGPUs); i++ {
		toShootdownGPU := accessingGPUs[i] - 1
		shootDownReq := protocol.NewShootdownCommand(
			d.gpuPort, d.GPUs[toShootdownGPU],
			vAddr, pid)
		d.requestsToSend = append(d.requestsToSend, shootDownReq)
	}

	return true
}

func (d *Driver) processShootdownCompleteRsp(
	req *protocol.ShootDownCompleteRsp,
) bool {
	d.numShootDownACK--

	if d.numShootDownACK == 0 {
		toRequestFromGPU := d.currentPageMigrationReq.CurrPageHostGPU
		toRequestFromPMCPort := d.RemotePMCPorts[toRequestFromGPU-1]

		migrationInfo := d.currentPageMigrationReq.MigrationInfo

		requestingGPUs := d.findRequestingGPUs(migrationInfo)
		context := d.findContext(d.currentPageMigrationReq.PID)

		pageVaddrs := make(map[uint64][]uint64)

		for i := 0; i < len(requestingGPUs); i++ {
			pageVaddrs[requestingGPUs[i]] =
				migrationInfo.GPUReqToVAddrMap[requestingGPUs[i]+1]
		}

		for gpuID, vAddrs := range pageVaddrs {
			for i := 0; i < len(vAddrs); i++ {
				vAddr := vAddrs[i]
				page, oldPAddr :=
					d.preparePageForMigration(vAddr, context, gpuID)

				req := protocol.NewPageMigrationReqToCP(d.gpuPort,
					d.GPUs[gpuID])
				req.DestinationPMCPort = toRequestFromPMCPort
				req.ToReadFromPhysicalAddress = oldPAddr
				req.ToWriteToPhysicalAddress = page.PAddr
				req.PageSize = d.currentPageMigrationReq.PageSize

				d.migrationReqToSendToCP = append(d.migrationReqToSendToCP, req)
				d.numPagesMigratingACK++
			}
		}
		return true
	}

	return false
}

func (d *Driver) findRequestingGPUs(
	migrationInfo *vm.PageMigrationInfo,
) []uint64 {
	requestingGPUs := make([]uint64, 0)

	for i := 1; i < d.GetNumGPUs()+1; i++ {
		_, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]
		if found {
			requestingGPUs = append(requestingGPUs, uint64(i-1))
		}
	}
	return requestingGPUs
}

func (d *Driver) findContext(pid vm.PID) *Context {
	context := &Context{}
	for i := 0; i < len(d.contexts); i++ {
		if d.contexts[i].pid == d.currentPageMigrationReq.PID {
			context = d.contexts[i]
		}
	}
	if context == nil {
		log.Panicf("Process does not exist")
	}
	return context
}

func (d *Driver) preparePageForMigration(
	vAddr uint64,
	context *Context,
	gpuID uint64,
) (*vm.Page, uint64) {
	page, found := d.pageTable.Find(context.pid, vAddr)
	if !found {
		panic("page not founds")
	}
	oldPAddr := page.PAddr

	newPage := d.memAllocator.AllocatePageWithGivenVAddr(
		context.pid, int(gpuID+1), vAddr, true)
	newPage.DeviceID = gpuID + 1

	newPage.IsMigrating = true
	d.pageTable.Update(newPage)

	return &newPage, oldPAddr
}

func (d *Driver) sendMigrationReqToCP() bool {
	if len(d.migrationReqToSendToCP) == 0 {
		return false
	}

	if d.isCurrentlyMigratingOnePage {
		return false
	}

	req := d.migrationReqToSendToCP[0]

	err := d.gpuPort.Send(req)
	if err == nil {
		d.migrationReqToSendToCP = d.migrationReqToSendToCP[1:]
		d.isCurrentlyMigratingOnePage = true
		return true
	}

	return false
}

func (d *Driver) processPageMigrationRspFromCP(
	rsp *protocol.PageMigrationRspToDriver,
) bool {
	d.numPagesMigratingACK--
	d.isCurrentlyMigratingOnePage = false

	if d.numPagesMigratingACK == 0 {
		d.prepareGPURestartReqs()
		d.preparePageMigrationRspToMMU()
	}

	return true
}

func (d *Driver) prepareGPURestartReqs() {
	accessingGPUs := d.currentPageMigrationReq.CurrAccessingGPUs

	for i := 0; i < len(accessingGPUs); i++ {
		restartGPUID := accessingGPUs[i] - 1
		restartReq := protocol.NewGPURestartReq(
			d.gpuPort,
			d.GPUs[restartGPUID])
		d.requestsToSend = append(d.requestsToSend, restartReq)
		d.numRestartACK++
	}
}

func (d *Driver) preparePageMigrationRspToMMU() {
	requestingGPUs := make([]uint64, 0)

	migrationInfo := d.currentPageMigrationReq.MigrationInfo

	for i := 1; i < d.GetNumGPUs()+1; i++ {
		_, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]
		if found {
			requestingGPUs = append(requestingGPUs, uint64(i-1))
		}
	}

	pageVaddrs := make(map[uint64][]uint64)

	for i := 0; i < len(requestingGPUs); i++ {
		pageVaddrs[requestingGPUs[i]] = migrationInfo.GPUReqToVAddrMap[requestingGPUs[i]+1]
	}

	req := vm.NewPageMigrationRspFromDriver(d.mmuPort.AsRemote(),
		d.currentPageMigrationReq.Src, d.currentPageMigrationReq)

	for _, vAddrs := range pageVaddrs {
		for j := 0; j < len(vAddrs); j++ {
			req.VAddr = append(req.VAddr, vAddrs[j])
		}
	}
	req.RspToTop = d.currentPageMigrationReq.RespondToTop
	d.toSendToMMU = req
}

func (d *Driver) handleGPURestartRsp(
	req *protocol.GPURestartRsp,
) bool {
	d.numRestartACK--
	if d.numRestartACK == 0 {
		d.prepareRDMARestartReqs()
	}
	return true
}

func (d *Driver) prepareRDMARestartReqs() {
	for i := 0; i < len(d.GPUs); i++ {
		req := protocol.NewRDMARestartCmdFromDriver(d.gpuPort, d.GPUs[i])
		d.requestsToSend = append(d.requestsToSend, req)
		d.numRDMARestartACK++
	}
}

func (d *Driver) processRDMARestartRspToDriver(
	rsp *protocol.RDMARestartRspToDriver) bool {
	d.numRDMARestartACK--

	if d.numRDMARestartACK == 0 {
		d.currentPageMigrationReq = nil
		d.isCurrentlyHandlingMigrationReq = false
		return true
	}
	return true
}

func (d *Driver) sendToMMU() bool {
	if d.toSendToMMU == nil {
		return false
	}
	req := d.toSendToMMU
	err := d.mmuPort.Send(req)
	if err == nil {
		d.toSendToMMU = nil
		return true
	}

	return false
}
