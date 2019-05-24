// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package run

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"
	"sync"

	"pkg/libs/atomic2"
	"pkg/libs/io/pipe"
	"pkg/libs/log"
	"pkg/redis"
	"redis-shake/base"
	"redis-shake/command"
	"redis-shake/common"
	"redis-shake/configure"
	"redis-shake/heartbeat"
	"redis-shake/metric"
)

type delayNode struct {
	t  time.Time // timestamp
	id int64     // id
}

type syncerStat struct {
	rbytes, wbytes, nentry, ignore int64

	forward, nbypass int64
}

type cmdDetail struct {
	Cmd  string
	Args [][]byte
}

func (c *cmdDetail) String() string {
	str := c.Cmd
	for _, s := range c.Args {
		str += " " + string(s)
	}
	return str
}

// main struct
type CmdSync struct {
	dbSyncers []*dbSyncer
}

// return send buffer length, delay channel length, target db offset
func (cmd *CmdSync) GetDetailedInfo() interface{} {
	ret := make([]map[string]interface{}, len(cmd.dbSyncers))
	for i, syncer := range cmd.dbSyncers {
		ret[i] = syncer.GetExtraInfo()
	}
	return ret
}

func (cmd *CmdSync) Main() {
	type syncNode struct {
		id             int
		source         string
		sourcePassword string
		target         string
		targetPassword string
	}

	// source redis number
	total := utils.GetTotalLink()
	syncChan := make(chan syncNode, total)
	cmd.dbSyncers = make([]*dbSyncer, total)
	for i, source := range conf.Options.SourceAddressList {
		// round-robin pick
		pick := utils.PickTargetRoundRobin(len(conf.Options.TargetAddressList))
		target := conf.Options.TargetAddressList[pick]

		nd := syncNode{
			id:             i,
			source:         source,
			sourcePassword: conf.Options.SourcePasswordRaw,
			target:         target,
			targetPassword: conf.Options.TargetPasswordRaw,
		}
		syncChan <- nd
	}

	var wg sync.WaitGroup
	wg.Add(len(conf.Options.SourceAddressList))

	for i := 0; i < int(conf.Options.SourceParallel); i++ {
		go func() {
			for {
				nd, ok := <-syncChan
				if !ok {
					break
				}

				ds := NewDbSyncer(nd.id, nd.source, nd.sourcePassword, nd.target, nd.targetPassword,
					conf.Options.HttpProfile + i)
				cmd.dbSyncers[nd.id] = ds
				log.Infof("routine[%v] starts syncing data from %v to %v with http[%v]",
					ds.id, ds.source, ds.target, ds.httpProfilePort)
				// run in routine
				go ds.sync()

				// wait full sync done
				<-ds.waitFull

				wg.Done()
			}
		}()
	}

	wg.Wait()
	close(syncChan)

	// never quit because increment syncing is still running
	select{}
}

/*------------------------------------------------------*/
// one sync link corresponding to one dbSyncer
func NewDbSyncer(id int, source, sourcePassword, target, targetPassword string, httpPort int) *dbSyncer {
	ds := &dbSyncer{
		id:              id,
		source:          source,
		sourcePassword:  sourcePassword,
		target:          target,
		targetPassword:  targetPassword,
		httpProfilePort: httpPort,
		waitFull:        make(chan struct{}),
	}

	// add metric
	metric.AddMetric(id)
	
	return ds
}

type dbSyncer struct {
	id int // current id in all syncer

	source         string // source address
	sourcePassword string // source password
	target         string // target address
	targetPassword string // target password

	httpProfilePort int // http profile port

	// metric info
	rbytes, wbytes, nentry, ignore atomic2.Int64
	forward, nbypass               atomic2.Int64
	targetOffset                   atomic2.Int64
	sourceOffset                   int64

	/*
	 * this channel is used to calculate delay between redis-shake and target redis.
	 * Once oplog sent, the corresponding delayNode push back into this queue. Next time
	 * receive reply from target redis, the front node poped and then delay calculated.
	 */
	delayChannel chan *delayNode

	sendBuf  chan cmdDetail // sending queue
	waitFull chan struct{}  // wait full sync done
}

func (ds *dbSyncer) GetExtraInfo() map[string]interface{} {
	return map[string]interface{}{
		"SourceAddress":      ds.source,
		"TargetAddress":      ds.target,
		"SenderBufCount":     len(ds.sendBuf),
		"ProcessingCmdCount": len(ds.delayChannel),
		"TargetDBOffset":     ds.targetOffset.Get(),
		"SourceDBOffset":     ds.sourceOffset,
	}
}

func (ds *dbSyncer) Stat() *syncerStat {
	return &syncerStat{
		rbytes: ds.rbytes.Get(),
		wbytes: ds.wbytes.Get(),
		nentry: ds.nentry.Get(),
		ignore: ds.ignore.Get(),

		forward: ds.forward.Get(),
		nbypass: ds.nbypass.Get(),
	}
}

func (ds *dbSyncer) sync() {
	var sockfile *os.File
	if len(conf.Options.SockFileName) != 0 {
		sockfile = utils.OpenReadWriteFile(conf.Options.SockFileName)
		defer sockfile.Close()
	}

	base.Status = "waitfull"
	var input io.ReadCloser
	var nsize int64
	if conf.Options.Psync {
		input, nsize = ds.sendPSyncCmd(ds.source, conf.Options.SourceAuthType, ds.sourcePassword)
	} else {
		input, nsize = ds.sendSyncCmd(ds.source, conf.Options.SourceAuthType, ds.sourcePassword)
	}
	defer input.Close()

	log.Infof("dbSyncer[%v] rdb file size = %d\n", ds.id, nsize)

	if sockfile != nil {
		r, w := pipe.NewFilePipe(int(conf.Options.SockFileSize), sockfile)
		defer r.Close()
		go func(r io.Reader) {
			defer w.Close()
			p := make([]byte, utils.ReaderBufferSize)
			for {
				utils.Iocopy(r, w, p, len(p))
			}
		}(input)
		input = r
	}

	// start heartbeat
	if len(conf.Options.HeartbeatUrl) > 0 {
		heartbeatCtl := heartbeat.HeartbeatController{
			ServerUrl: conf.Options.HeartbeatUrl,
			Interval:  int32(conf.Options.HeartbeatInterval),
		}
		go heartbeatCtl.Start()
	}

	reader := bufio.NewReaderSize(input, utils.ReaderBufferSize)

	// sync rdb
	base.Status = "full"
	ds.syncRDBFile(reader, ds.target, conf.Options.TargetAuthType, ds.targetPassword, nsize)

	// sync increment
	base.Status = "incr"
	close(ds.waitFull)
	ds.syncCommand(reader, ds.target, conf.Options.TargetAuthType, ds.targetPassword)
}

func (ds *dbSyncer) sendSyncCmd(master, auth_type, passwd string) (net.Conn, int64) {
	c, wait := utils.OpenSyncConn(master, auth_type, passwd)
	for {
		select {
		case nsize := <-wait:
			if nsize == 0 {
				log.Infof("dbSyncer[%v] +", ds.id)
			} else {
				return c, nsize
			}
		case <-time.After(time.Second):
			log.Infof("dbSyncer[%v] -", ds.id)
		}
	}
}

func (ds *dbSyncer) sendPSyncCmd(master, auth_type, passwd string) (pipe.Reader, int64) {
	c := utils.OpenNetConn(master, auth_type, passwd)
	log.Infof("dbSyncer[%v] psync connect '%v' with auth type[%v] OK!", ds.id, master, auth_type)

	utils.SendPSyncListeningPort(c, conf.Options.HttpProfile)
	log.Infof("dbSyncer[%v] psync send listening port[%v] OK!", ds.id, conf.Options.HttpProfile)

	// reader buffer bind to client
	br := bufio.NewReaderSize(c, utils.ReaderBufferSize)
	// writer buffer bind to client
	bw := bufio.NewWriterSize(c, utils.WriterBufferSize)

	log.Infof("dbSyncer[%v] try to send 'psync' command", ds.id)
	// send psync command and decode the result
	runid, offset, wait := utils.SendPSyncFullsync(br, bw)
	ds.targetOffset.Set(offset)
	log.Infof("dbSyncer[%v] psync runid = %s offset = %d, fullsync", ds.id, runid, offset)

	// get rdb file size
	var nsize int64
	for nsize == 0 {
		select {
		case nsize = <-wait:
			if nsize == 0 {
				log.Infof("dbSyncer[%v] +", ds.id)
			}
		case <-time.After(time.Second):
			log.Infof("dbSyncer[%v] -", ds.id)
		}
	}

	// write -> pipew -> piper -> read
	piper, pipew := pipe.NewSize(utils.ReaderBufferSize)

	go func() {
		defer pipew.Close()
		p := make([]byte, 8192)
		// read rdb in for loop
		for rdbsize := int(nsize); rdbsize != 0; {
			// br -> pipew
			rdbsize -= utils.Iocopy(br, pipew, p, rdbsize)
		}

		for {
			/*
			 * read from br(source redis) and write into pipew.
			 * Generally speaking, this function is forever run.
			 */
			n, err := ds.pSyncPipeCopy(c, br, bw, offset, pipew)
			if err != nil {
				log.PanicErrorf(err, "dbSyncer[%v] psync runid = %s, offset = %d, pipe is broken",
					ds.id, runid, offset)
			}
			// the 'c' is closed every loop

			offset += n
			ds.targetOffset.Set(offset)

			// reopen 'c' every time
			for {
				// ds.SyncStat.SetStatus("reopen")
				base.Status = "reopen"
				time.Sleep(time.Second)
				c = utils.OpenNetConnSoft(master, auth_type, passwd)
				if c != nil {
					// log.PurePrintf("%s\n", NewLogItem("SourceConnReopenSuccess", "INFO", LogDetail{Info: strconv.FormatInt(offset, 10)}))
					log.Infof("dbSyncer[%v] Event:SourceConnReopenSuccess\tId: %s\toffset = %d",
						ds.id, conf.Options.Id, offset)
					// ds.SyncStat.SetStatus("incr")
					base.Status = "incr"
					break
				} else {
					// log.PurePrintf("%s\n", NewLogItem("SourceConnReopenFail", "WARN", NewErrorLogDetail("", "")))
					log.Errorf("dbSyncer[%v] Event:SourceConnReopenFail\tId: %s", ds.id, conf.Options.Id)
				}
			}
			utils.AuthPassword(c, auth_type, passwd)
			utils.SendPSyncListeningPort(c, conf.Options.HttpProfile)
			br = bufio.NewReaderSize(c, utils.ReaderBufferSize)
			bw = bufio.NewWriterSize(c, utils.WriterBufferSize)
			utils.SendPSyncContinue(br, bw, runid, offset)
		}
	}()
	return piper, nsize
}

func (ds *dbSyncer) pSyncPipeCopy(c net.Conn, br *bufio.Reader, bw *bufio.Writer, offset int64, copyto io.Writer) (int64, error) {
	// TODO, two times call c.Close() ? maybe a bug
	defer c.Close()
	var nread atomic2.Int64
	go func() {
		defer c.Close()
		for {
			time.Sleep(time.Second * 1)
			select {
			case <-ds.waitFull:
				if err := utils.SendPSyncAck(bw, offset+nread.Get()); err != nil {
					return
				}
			default:
				if err := utils.SendPSyncAck(bw, 0); err != nil {
					return
				}
			}
		}
	}()

	var p = make([]byte, 8192)
	for {
		n, err := br.Read(p)
		if err != nil {
			return nread.Get(), nil
		}
		if _, err := copyto.Write(p[:n]); err != nil {
			return nread.Get(), err
		}
		nread.Add(int64(n))
	}
}

func (ds *dbSyncer) syncRDBFile(reader *bufio.Reader, target, auth_type, passwd string, nsize int64) {
	pipe := utils.NewRDBLoader(reader, &ds.rbytes, base.RDBPipeSize)
	wait := make(chan struct{})
	go func() {
		defer close(wait)
		var wg sync.WaitGroup
		wg.Add(conf.Options.Parallel)
		for i := 0; i < conf.Options.Parallel; i++ {
			go func() {
				defer wg.Done()
				c := utils.OpenRedisConn(target, auth_type, passwd)
				defer c.Close()
				var lastdb uint32 = 0
				for e := range pipe {
					if !base.AcceptDB(e.DB) {
						ds.ignore.Incr()
					} else {
						ds.nentry.Incr()
						if conf.Options.TargetDB != -1 {
							if conf.Options.TargetDB != int(lastdb) {
								lastdb = uint32(conf.Options.TargetDB)
								utils.SelectDB(c, uint32(conf.Options.TargetDB))
							}
						} else {
							if e.DB != lastdb {
								lastdb = e.DB
								utils.SelectDB(c, lastdb)
							}
						}

						if len(conf.Options.FilterKey) != 0 {
							for i := 0; i < len(conf.Options.FilterKey); i++ {
								if strings.HasPrefix(string(e.Key), conf.Options.FilterKey[i]) {
									utils.RestoreRdbEntry(c, e)
									break
								}
							}
						} else if len(conf.Options.FilterSlot) > 0 {
							for _, slot := range conf.Options.FilterSlot {
								slotInt, _ := strconv.Atoi(slot)
								if int(utils.KeyToSlot(string(e.Key))) == slotInt {
									utils.RestoreRdbEntry(c, e)
									break
								}
							}
						} else {
							utils.RestoreRdbEntry(c, e)
						}
					}
				}
			}()
		}

		wg.Wait()
	}()

	var stat *syncerStat

	for done := false; !done; {
		select {
		case <-wait:
			done = true
		case <-time.After(time.Second):
		}
		stat = ds.Stat()
		var b bytes.Buffer
		fmt.Fprintf(&b, "dbSyncer[%v] total=%d - %12d [%3d%%]  entry=%-12d",
			ds.id, nsize, stat.rbytes, 100*stat.rbytes/nsize, stat.nentry)
		if stat.ignore != 0 {
			fmt.Fprintf(&b, "  ignore=%-12d", stat.ignore)
		}
		log.Info(b.String())
		metric.GetMetric(ds.id).SetFullSyncProgress(uint64(100 * stat.rbytes / nsize))
	}
	log.Infof("dbSyncer[%v] sync rdb done", ds.id)
}

func (ds *dbSyncer) syncCommand(reader *bufio.Reader, target, auth_type, passwd string) {
	c := utils.OpenRedisConnWithTimeout(target, auth_type, passwd, time.Duration(10)*time.Minute, time.Duration(10)*time.Minute)
	defer c.Close()

	ds.sendBuf = make(chan cmdDetail, conf.Options.SenderCount)
	ds.delayChannel = make(chan *delayNode, conf.Options.SenderDelayChannelSize)
	var sendId, recvId, sendMarkId atomic2.Int64 // sendMarkId is also used as mark the sendId in sender routine

	go func() {
		if conf.Options.Psync == false {
			log.Warnf("dbSyncer[%v] GetFakeSlaveOffset not enable when psync == false", ds.id)
			return
		}

		srcConn := utils.OpenRedisConnWithTimeout(ds.source, conf.Options.SourceAuthType, ds.sourcePassword,
			time.Duration(10)*time.Minute, time.Duration(10)*time.Minute)
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			offset, err := utils.GetFakeSlaveOffset(srcConn)
			if err != nil {
				// log.PurePrintf("%s\n", NewLogItem("GetFakeSlaveOffsetFail", "WARN", NewErrorLogDetail("", err.Error())))
				log.Warnf("dbSyncer[%v] Event:GetFakeSlaveOffsetFail\tId:%s\tWarn:%s",
					ds.id, conf.Options.Id, err.Error())

				// Reconnect while network error happen
				if err == io.EOF {
					srcConn = utils.OpenRedisConnWithTimeout(ds.source, conf.Options.SourceAuthType,
						ds.sourcePassword, time.Duration(10)*time.Minute, time.Duration(10)*time.Minute)
				} else if _, ok := err.(net.Error); ok {
					srcConn = utils.OpenRedisConnWithTimeout(ds.source, conf.Options.SourceAuthType,
						ds.sourcePassword, time.Duration(10)*time.Minute, time.Duration(10)*time.Minute)
				}
			} else {
				// ds.SyncStat.SetOffset(offset)
				if ds.sourceOffset, err = strconv.ParseInt(offset, 10, 64); err != nil {
					log.Errorf("dbSyncer[%v] Event:GetFakeSlaveOffsetFail\tId:%s\tError:%s",
						ds.id, conf.Options.Id, err.Error())
				}
			}
			// ds.SyncStat.SendBufCount = int64(len(sendBuf))
			// ds.SyncStat.ProcessingCmdCount = int64(len(ds.delayChannel))
			//log.Infof("%s", ds.SyncStat.Roll())
			// ds.SyncStat.Roll()
			// log.PurePrintf("%s\n", NewLogItem("Metric", "INFO", ds.SyncStat.Snapshot()))
		}
	}()

	go func() {
		var node *delayNode
		for {
			reply, err := c.Receive()

			recvId.Incr()
			id := recvId.Get() // receive id

			// print debug log of receive reply
			log.Debugf("receive reply[%v]: [%v], error: [%v]", id, reply, err)

			if conf.Options.Metric == false {
				continue
			}

			if err == nil {
				metric.GetMetric(ds.id).AddSuccessCmdCount(1)
			} else {
				metric.GetMetric(ds.id).AddFailCmdCount(1)
				if utils.CheckHandleNetError(err) {
					log.Panicf("dbSyncer[%v] Event:NetErrorWhileReceive\tId:%s\tError:%s",
						ds.id, conf.Options.Id, err.Error())
				} else {
					log.Panicf("dbSyncer[%v] Event:ErrorReply\tId:%s\tCommand: [unknown]\tError: %s",
						ds.id, conf.Options.Id, err.Error())
				}
			}

			if node == nil {
				// non-blocking read from delay channel
				select {
				case node = <-ds.delayChannel:
				default:
					// it's ok, channel is empty
				}
			}

			if node != nil {
				if node.id == id {
					metric.GetMetric(ds.id).AddDelay(uint64(time.Now().Sub(node.t).Nanoseconds()) / 1000000) // ms
					node = nil
				} else if node.id < id {
					log.Panicf("dbSyncer[%v] receive id invalid: node-id[%v] < receive-id[%v]",
						ds.id, node.id, id)
				}
			}
		}
	}()

	go func() {
		var lastdb int32 = 0
		var bypass bool = false
		var isselect bool = false

		var scmd string
		var argv, new_argv [][]byte
		var err error

		decoder := redis.NewDecoder(reader)

		log.Infof("dbSyncer[%v] Event:IncrSyncStart\tId:%s\t", ds.id, conf.Options.Id)

		for {
			ignorecmd := false
			isselect = false
			resp := redis.MustDecodeOpt(decoder)

			if scmd, argv, err = redis.ParseArgs(resp); err != nil {
				log.PanicErrorf(err, "dbSyncer[%v] parse command arguments failed", ds.id)
			} else {
				metric.GetMetric(ds.id).AddPullCmdCount(1)

				// print debug log of send command
				if conf.Options.LogLevel == utils.LogLevelAll {
					strArgv := make([]string, len(argv))
					for i, ele := range argv {
						strArgv[i] = *(*string)(unsafe.Pointer(&ele))
					}
					sendMarkId.Incr()
					log.Debugf("dbSyncer[%v] send command[%v]: [%s %v]", ds.id, sendMarkId.Get(), scmd, strArgv)
				}

				if scmd != "ping" {
					if strings.EqualFold(scmd, "select") {
						if len(argv) != 1 {
							log.Panicf("dbSyncer[%v] select command len(args) = %d", ds.id, len(argv))
						}
						s := string(argv[0])
						n, err := strconv.Atoi(s)
						if err != nil {
							log.PanicErrorf(err, "dbSyncer[%v] parse db = %s failed", ds.id, s)
						}
						bypass = !base.AcceptDB(uint32(n))
						isselect = true
					} else if strings.EqualFold(scmd, "opinfo") {
						ignorecmd = true
					}
					if bypass || ignorecmd {
						ds.nbypass.Incr()
						// ds.SyncStat.BypassCmdCount.Incr()
						metric.GetMetric(ds.id).AddBypassCmdCount(1)
						continue
					}
				}

				is_filter := false
				if len(conf.Options.FilterKey) != 0 {
					ds, ok := command.RedisCommands[scmd]
					if ok && len(argv) > 0 {
						new_argv, is_filter = command.GetMatchKeys(ds, argv, conf.Options.FilterKey)
					} else {
						is_filter = true
						new_argv = argv
					}
				} else {
					is_filter = true
					new_argv = argv
				}
				if bypass || ignorecmd || !is_filter {
					ds.nbypass.Incr()
					continue
				}
			}

			if isselect && conf.Options.TargetDB != -1 {
				if conf.Options.TargetDB != int(lastdb) {
					lastdb = int32(conf.Options.TargetDB)
					//sendBuf <- cmdDetail{Cmd: scmd, Args: argv, Timestamp: time.Now()}
					/* send select command. */
					ds.sendBuf <- cmdDetail{Cmd: "SELECT", Args: [][]byte{[]byte(strconv.FormatInt(int64(lastdb), 10))}}
				} else {
					ds.nbypass.Incr()
					metric.GetMetric(ds.id).AddBypassCmdCount(1)
				}
				continue
			}
			ds.sendBuf <- cmdDetail{Cmd: scmd, Args: new_argv}
		}
	}()

	go func() {
		var noFlushCount uint
		var cachedSize uint64

		for item := range ds.sendBuf {
			length := len(item.Cmd)
			data := make([]interface{}, len(item.Args))
			for i := range item.Args {
				data[i] = item.Args[i]
				length += len(item.Args[i])
			}
			err := c.Send(item.Cmd, data...)
			if err != nil {
				log.Panicf("dbSyncer[%v] Event:SendToTargetFail\tId:%s\tError:%s\t",
					ds.id, conf.Options.Id, err.Error())
			}
			noFlushCount += 1

			ds.forward.Incr()
			metric.GetMetric(ds.id).AddPushCmdCount(1)
			metric.GetMetric(ds.id).AddNetworkFlow(uint64(length))
			sendId.Incr()

			if conf.Options.Metric {
				// delay channel
				ds.addDelayChan(sendId.Get())
			}

			if noFlushCount > conf.Options.SenderCount || cachedSize > conf.Options.SenderSize ||
					len(ds.sendBuf) == 0 { // 5000 ds in a batch
				err := c.Flush()
				noFlushCount = 0
				cachedSize = 0
				if utils.CheckHandleNetError(err) {
					log.Panicf("dbSyncer[%v] Event:NetErrorWhileFlush\tId:%s\tError:%s\t",
						ds.id, conf.Options.Id, err.Error())
				}
			}
		}
	}()

	for lstat := ds.Stat(); ; {
		time.Sleep(time.Second)
		nstat := ds.Stat()
		var b bytes.Buffer
		fmt.Fprintf(&b, "dbSyncer[%v] sync: ", ds.id)
		fmt.Fprintf(&b, " +forward=%-6d", nstat.forward-lstat.forward)
		fmt.Fprintf(&b, " +nbypass=%-6d", nstat.nbypass-lstat.nbypass)
		fmt.Fprintf(&b, " +nbytes=%d", nstat.wbytes-lstat.wbytes)
		log.Info(b.String())
		lstat = nstat
	}
}

func (ds *dbSyncer) addDelayChan(id int64) {
	// send
	/*
	 * available >=4096: 1:1 sampling
	 * available >=1024: 1:10 sampling
	 * available >=128: 1:100 sampling
	 * else: 1:1000 sampling
	 */
	used := cap(ds.delayChannel) - len(ds.delayChannel)
	if used >= 4096 ||
		used >= 1024 && id%10 == 0 ||
		used >= 128 && id%100 == 0 ||
		id%1000 == 0 {
		// non-blocking add
		select {
		case ds.delayChannel <- &delayNode{t: time.Now(), id: id}:
		default:
			// do nothing but print when channel is full
			log.Warnf("dbSyncer[%v] delayChannel is full", ds.id)
		}
	}
}
