package main

import (
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	//"time"
	//"errors"
	//"fmt"
	"runtime/debug"
	"time"
)

type BufferflowGrbl struct {
	Name           			string
	Port           			string
	parent_serport 			*serport

	Paused       			bool
	ManualPaused 			bool // indicates user hard paused the buffer on their own, i.e. not from flow control

	BufferMax 				int
	availableBufferSpace 	int
	q         				*Queue

	sem 					chan int // semaphore to wait on until given release

	LatestData 				string // this holds the latest data across multiple serial reads so we can analyze it for qr responses
	LastStatus 				string //do we need this?

	version 				string

	quit 					chan int

	reNewLine    			*regexp.Regexp
	ok           			*regexp.Regexp
	err          			*regexp.Regexp
	initline     			*regexp.Regexp
	qry          			*regexp.Regexp
	rpt          			*regexp.Regexp
	reComment    			*regexp.Regexp
	reComment2   			*regexp.Regexp
	statusReport 			*regexp.Regexp
	reNoResponse 			*regexp.Regexp
	buf          			*regexp.Regexp
	statusConfig 			*regexp.Regexp
	
	
	lock 					*sync.Mutex  // use thread locking for b.Paused
	manualLock 				*sync.Mutex  // use thread locking for b.ManualPaused
	semLock 				*sync.Mutex  // use more thread locking for b.semLock
}

func (b *BufferflowGrbl) Init() {

	b.Paused = false
	b.ManualPaused = false
	log.Println("Initting GRBL buffer flow")
	// b.BufferMax = 127 //max buffer size 127 bytes available
	b.BufferMax = 125 // changed to be safe with extra chars
	b.lock = &sync.Mutex{}
	b.manualLock = &sync.Mutex{}
	b.semLock = &sync.Mutex{}
	//b.SetPaused(false, 2)
	b.q = NewQueue()

	// make buffered channel big enough we won't overflow it
	// meaning we get told b.sem on incoming data, so at most this could
	// be the size of 1 character and the TinyG only allows 255, so just
	// go high to make sure it's high enough to never block
	// buffered
	b.sem = make(chan int, 1000)

	b.availableBufferSpace = b.BufferMax

	//define regex
	b.reNewLine, _ = regexp.Compile("\\r{0,1}\\n{1,2}") //\\r{0,1}
	b.ok, _ = regexp.Compile("^ok")
	b.err, _ = regexp.Compile("^error")
	b.initline, _ = regexp.Compile("^Grbl v?(.+) .*")
	b.qry, _ = regexp.Compile("\\?")
	b.rpt, _ = regexp.Compile("^<")
	b.statusReport, _ = regexp.Compile("^\\$10=1")
	b.buf, _ = regexp.Compile("Bf:[0-9]{1,3},([0-9]{1,3})")
	b.statusConfig, _ = regexp.Compile("^\\$10=1")

	// this regexp catches !, ~, %, \n, $ by itself, or $$ by itself and indicates
	// no response will come back so don't expect it
	b.reNoResponse, _ = regexp.Compile("^[!~%\n$?]")

	b.reComment, _ = regexp.Compile("\\(.*?\\)")	//to get rid of comments
	b.reComment2, _ = regexp.Compile(";.*")    

	//initialize query loop
	//b.rxQueryLoop(b.parent_serport)
	
	b.rptQueryLoop(b.parent_serport)

}

func (b *BufferflowGrbl) GetManualPaused() bool {
	b.manualLock.Lock()
	defer b.manualLock.Unlock()
	return b.ManualPaused
}

func (b *BufferflowGrbl) SetManualPaused(isPaused bool) {
	b.manualLock.Lock()
	defer b.manualLock.Unlock()
	b.ManualPaused = isPaused
}

//	Sets the paused state of this buffer
//	go-routine safe.
func (b *BufferflowGrbl) SetPaused(isPaused bool, semRelease int) {
	b.lock.Lock()
	defer b.lock.Unlock()
	b.Paused = isPaused

	// only release semaphore if we are being told to unpause
	if b.Paused == false {
		b.sem <- semRelease
		log.Println("Just sent release to b.sem so we will not block the sending to serial port anymore.")
	}
}

func (b *BufferflowGrbl) Close() {
	//stop the rx query loop when the serial port is closed off.
	log.Println("Stopping the RX query loop")
	b.ReleaseLock()
	b.Unpause()
	go func() {
		b.quit <- 1
	}()
}

//	Gets the paused state of this buffer
//	go-routine safe.
func (b *BufferflowGrbl) GetPaused() bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.Paused
}

//Use this function to open a connection, write directly to serial port and close connection.
//This is used for sending query requests outside of the normal buffered operations that will pause to wait for room in the grbl buffer
//'?' is asynchronous to the normal buffer load and does not need to be paused when buffer full
func (b *BufferflowGrbl) rptQueryLoop(p *serport) {
	b.parent_serport = p //make note of this port for use in clearing the buffer later, on error.
	ticker := time.NewTicker(250 * time.Millisecond)
	b.quit = make(chan int)
	go func() {
		for {
			select {
			case <-ticker.C:

				n2, err := p.portIo.Write([]byte("?"))

				log.Print("Just wrote ", n2, " bytes to serial: ?")

				if err != nil {
					errstr := "Error writing to " + p.portConf.Name + " " + err.Error() + " Closing port."
					log.Print(errstr)
					h.broadcastSys <- []byte(errstr)
					ticker.Stop() //stop query loop if we can't write to the port
					break
				}
			case <-b.quit:
				ticker.Stop()
				return
			}
		}
	}()
}

//Use this function to open a connection, write directly to serial port and close connection.
//This is used for sending query requests outside of the normal buffered operations that will pause to wait for room in the grbl buffer
//'?' is asynchronous to the normal buffer load and does not need to be paused when buffer full
func (b *BufferflowGrbl) rxQueryLoop(p *serport) {
	b.parent_serport = p //make note of this port for use in clearing the buffer later, on error.
	ticker := time.NewTicker(5000 * time.Millisecond)
	b.quit = make(chan int)
	go func() {
		for {
			select {
			case <-ticker.C:

				n2, err := p.portIo.Write([]byte("?"))

				log.Print("Just wrote ", n2, " bytes to serial: ?")

				if err != nil {
					errstr := "Error writing to " + p.portConf.Name + " " + err.Error() + " Closing port."
					log.Print(errstr)
					h.broadcastSys <- []byte(errstr)
					ticker.Stop() //stop query loop if we can't write to the port
					break
				}
			case <-b.quit:
				ticker.Stop()
				return
			}
		}
	}()
}

func (b *BufferflowGrbl) IsBufferGloballySendingBackIncomingData() bool {
	// we want to send back incoming data as per line data
	// rather than having the default spjs implemenation that sends back data
	// as it sees it. the reason is that we were getting packets out of order
	// on the browser on bad internet connections. that will still happen with us
	// sending back per line data, but at least it will allow the browser to parse
	// correct json now.
	// TODO: The right way to solve this is to watch for an acknowledgement
	// from the browser and queue stuff up until the acknowledgement and then
	// send the full blast of ganged up data
	return true
}

// This is called if user wiped entire buffer of gcode commands queued up
// which is up to 25,000 of them. So, we need to release the OnBlockUntilReady()
// in a way where the command will not get executed, so send unblockType of 2
func (b *BufferflowGrbl) ReleaseLock() {
	log.Println("Lock being released in TinyG buffer")
	b.q.Delete()
	b.SetPaused(false, 2)
}

func (b *BufferflowGrbl) SeeIfSpecificCommandsReturnNoResponse(cmd string) bool {
	if match := b.reNoResponse.MatchString(cmd); match {
		//log.Printf("Found cmd that does not get a response from Grbl. cmd:%v\n", cmd)
		return true
	}
	return false
}

//either cancel or % will wipe buffer
func (b *BufferflowGrbl) SeeIfSpecificCommandsShouldWipeBuffer(cmd string) bool {
	// remove comments
	cmd = b.reComment.ReplaceAllString(cmd, "")
	cmd = b.reComment2.ReplaceAllString(cmd, "")
	if match, _ := regexp.MatchString("[%\u0018]", cmd); match {
		return true
	}
	return false
}

func (b *BufferflowGrbl) SeeIfSpecificCommandsShouldSkipBuffer(cmd string) bool {
	// remove comments
	cmd = b.reComment.ReplaceAllString(cmd, "")
	cmd = b.reComment2.ReplaceAllString(cmd, "")
	// adding some new regexp to match real-time commands for grbl 1 version
	if match, _ := regexp.MatchString("[!~\\?]|(\u0018)|[\u0080-\u00FF]", cmd); match {
		log.Printf("Found cmd that should skip buffer. cmd:%v\n", cmd)
		return true
	}
	return false
}

func (b *BufferflowGrbl) SeeIfSpecificCommandsShouldPauseBuffer(cmd string) bool {
	// remove comments
	cmd = b.reComment.ReplaceAllString(cmd, "")
	cmd = b.reComment2.ReplaceAllString(cmd, "")
	if match, _ := regexp.MatchString("[!]", cmd); match {
		//log.Printf("Found cmd that should pause buffer. cmd:%v\n", cmd)
		return true
	}
	return false
}

func (b *BufferflowGrbl) SeeIfSpecificCommandsShouldUnpauseBuffer(cmd string) bool {
	// remove comments
	cmd = b.reComment.ReplaceAllString(cmd, "")
	cmd = b.reComment2.ReplaceAllString(cmd, "")
	/*  query: should we include % too */
	if match, _ := regexp.MatchString("[~]", cmd); match {
		//log.Printf("Found cmd that should unpause buffer. cmd:%v\n", cmd)
		return true
	}
	return false
}

func (b *BufferflowGrbl) Pause() {
	b.SetPaused(true, 0) //b.Paused = true
	log.Println("Paused buffer")
}

func (b *BufferflowGrbl) Unpause() {
	b.SetPaused(false, 1)          //b.Paused = false
	log.Println("Unpaused buffer") // inside BlockUntilReady() call")
}

func (b *BufferflowGrbl) BreakApartCommands(cmd string) []string {

	// add newline after !~%
	log.Printf("Command Before Break-Apart: %q\n", cmd)

	cmds := strings.Split(cmd, "\n")
	finalCmds := []string{}
	for _, item := range cmds {
		//remove comments and whitespace from item
		item = regexp.MustCompile("\\(.*?\\)").ReplaceAllString(item, "")
		item = regexp.MustCompile(";.*").ReplaceAllString(item, "")
		item = strings.Replace(item, " ", "", -1)

		if item == "*init*" { //return init string to update grbl widget when already connected to grbl
			m := DataPerLine{b.Port, b.version + "\n"}
			bm, err := json.Marshal(m)
			if err == nil {
				h.broadcastSys <- bm
			}
		} else if item == "*status*" { //return status when client first connects to existing open port
			m := DataPerLine{b.Port, b.LastStatus + "\n"}
			bm, err := json.Marshal(m)
			if err == nil {
				h.broadcastSys <- bm
			}
		} else if item == "?" {
			log.Printf("Query added without newline: %q\n", item)
			finalCmds = append(finalCmds, item) //append query request without newline character
		} else if item == "%" {
			log.Printf("Wiping Grbl BufferFlow")
			//b.LocalBufferWipe(b.parent_serport)
			//dont add this command to the list of finalCmds
		} else if b.statusConfig.MatchString(item) {
			log.Printf("Ignoring command to suppress reporting")
		} else if item != "" {
			log.Printf("Re-adding newline to item:%v\n", item)
			s := item + "\n"
			finalCmds = append(finalCmds, s)
			log.Printf("New cmd item:%v\n", s)
		}

	}
	log.Printf("Final array of cmds after BreakApartCommands(). finalCmds:%v\n", finalCmds)

	return finalCmds
	//return []string{cmd} //do not process string
}

// Clean out b.sem so it can truly block
func (b *BufferflowGrbl) ClearOutSemaphore() {
	ctr := 0

	keepLooping := true
	for keepLooping {
		select {
		case _, ok := <-b.sem: // case d, ok :=
			//log.Printf("Consuming b.sem queue to clear it before we block. ok:%v, d:%v\n", ok, string(d))
			ctr++
			if ok == false {
				keepLooping = false
			}
		default:
			keepLooping = false
			//log.Println("Hit default in select clause")
		}
	}
	//log.Printf("Done consuming b.sem queue so we're good to block on it now. ctr:%v\n", ctr)
	// ok, all b.sem signals are now consumed into la-la land

}

func (b *BufferflowGrbl) RewriteSerialData(cmd string, id string) string {
	return ""
}

func (b *BufferflowGrbl) BlockUntilReady(cmd string, id string) (bool, bool, string) {
	log.Printf("BlockUntilReady(cmd:%v, id:%v) start\n", cmd, id)

	// Since BlockUntilReady is in the writer thread, lock so the reader
	// thread doesn't get messed up from all the bufferarray counting we're doing
	//b.lock.Lock()
	//defer b.lock.Unlock()

	// Here we add the length of the new command to the buffer size and append the length
	// to the buffer array.  Check if buffersize > buffermax and if so we pause and await free space before
	// sending the command to grbl.

	// Only increment if cmd is something we'll get an r:{} response to
	isReturnsNoResponse := b.SeeIfSpecificCommandsReturnNoResponse(cmd)
	if isReturnsNoResponse == false {

		b.q.Push(cmd, id)
		/*
			log.Printf("Going to lock inside BlockUntilReady to up the BufferSize and Arrays\n")
			b.lock.Lock()
			b.BufferSize += len(cmd)
			b.BufferSizeArray = append(b.BufferSizeArray, len(cmd))
			b.BufferCmdArray = append(b.BufferCmdArray, cmd)
			b.lock.Unlock()
			log.Printf("Done locking inside BlockUntilReady to up the BufferSize and Arrays\n")
		*/
	} else {
		// this is sketchy. could we overrun the buffer by not counting !~%\n
		// so to give extra room don't actually allow full serial buffer to
		// be used in b.BufferMax
		//log.Printf("Not incrementing buffer size for cmd:%v\n", cmd)

	}

	log.Printf("New line length: %v, buffer size increased to:%v\n", len(cmd), b.q.LenOfCmds())
	//log.Println(b.BufferSizeArray)
	//log.Println(b.BufferCmdArray)

	//b.lock.Lock()
	if b.q.LenOfCmds() >= b.BufferMax {
		b.SetPaused(true, 0) // b.Paused = true
		log.Printf("It looks like the buffer is over the allowed size, so we are going to pause. Then when some incoming responses come in a check will occur to see if there's room to send this command. Pausing...")
	}
	//b.lock.Lock()

	if b.GetPaused() {
		//log.Println("It appears we are being asked to pause, so we will wait on b.sem")
		// We are being asked to pause our sending of commands

		// clear all b.sem signals so when we block below, we truly block
		b.ClearOutSemaphore()

		log.Println("Blocking on b.sem until told from OnIncomingData to go")
		unblockType, ok := <-b.sem // will block until told from OnIncomingData to go

		log.Printf("Done blocking cuz got b.sem semaphore release. ok:%v, unblockType:%v\n", ok, unblockType)

		// we get an unblockType of 1 for normal unblocks
		// we get an unblockType of 2 when we're being asked to wipe the buffer, i.e. from a % cmd
		if unblockType == 2 {
			log.Println("This was an unblock of type 2, which means we're being asked to wipe internal buffer. so return false.")
			// returning false asks the calling method to wipe the serial send once
			// this function returns
			return false, false, ""
		}
	}

	// we will get here when we're done blocking and if we weren't cancelled
	// if this cmd returns no response, we need to generate a fake "Complete"
	// so do it now
	willHandleCompleteResponse := true
	if isReturnsNoResponse == true {
		willHandleCompleteResponse = false
	}

	//log.Printf("BlockUntilReady(cmd:%v, id:%v) end\n", cmd, id)

	return true, willHandleCompleteResponse, ""
}

func (b *BufferflowGrbl) OnIncomingData(data string) {
	//log.Printf("OnIncomingData() start. data:%q\n", data)
	//log.Printf("< %q\n", data)

	// Since OnIncomingData is in the reader thread, lock so the writer
	// thread doesn't get messed up from all the bufferarray counting we're doing
	//b.lock.Lock()
	//defer b.lock.Unlock()

	b.LatestData += data

	arrLines := b.reNewLine.Split(b.LatestData, -1)

	if len(arrLines) > 1 {
		// that means we found a newline and have 2 or greater array values
		// so we need to analyze our arrLines[] lines but keep last line
		// for next trip into OnIncomingData
		log.Printf("We have data lines to analyze. numLines:%v\n", len(arrLines))
	} else {
		// we don't have a newline yet, so just exit and move on
		// we don't have to reset b.LatestData because we ended up
		// without any newlines so maybe we will next time into this method
		log.Printf("Did not find newline yet, so nothing to analyze\n")
		return
	}

	// if we made it here we have lines to analyze
	// so analyze all of them except the last line
	for _, element := range arrLines[:len(arrLines)-1] {
		//log.Printf("Working on element:%v, index:%v", element, index)
		//log.Printf("Working on element:%v, index:%v", element)
		log.Printf("< %v", element)

		//check for r:{} response indicating a gcode line has been processed

		if b.ok.MatchString(element) || b.err.MatchString(element) {
			//log.Printf("Going to lock inside OnIncomingData to decrease the BufferSize and reset Arrays\n")
			//b.lock.Lock()

			//if b.BufferSizeArray != nil {
			// ok, a line has been processed, the if statement below better
			// be guaranteed to be true, cuz if its not we did something wrong
			if b.q.Len() > 0 {
				doneCmd, id := b.q.Poll()

				if b.ok.MatchString(element) {
					// Send cmd:"Complete" back
					m := DataCmdComplete{"Complete", id, b.Port, b.q.LenOfCmds(), doneCmd}
					bm, err := json.Marshal(m)
					if err == nil {
						h.broadcastSys <- bm
					}
				} else if b.err.MatchString(element) {
					// Send cmd:"Error" back
					log.Printf("Error Response Received:%v, id:%v", doneCmd, id)
					m := DataCmdComplete{"Error", id, b.Port, b.q.LenOfCmds(), doneCmd}
					bm, err := json.Marshal(m)
					if err == nil {
						h.broadcastSys <- bm
					}
				}
				log.Printf("Buffer decreased to itemCnt:%v, lenOfBuf:%v\n", b.q.Len(), b.q.LenOfCmds())
			} else {
				log.Printf("We should NEVER get here cuz we should have a command in the queue to dequeue when we get any response. If you see this debug stmt this is BAD!!!!")
			}

			//if b.BufferSize < b.BufferMax {
			// We should have our queue dequeued so lets see if we are now below
			// the allowed buffer room. If so go ahead and release the block on send
			// This if stmt still may not be true here because we could have had a tiny
			// cmd just get completed like "G0 X0" and the next cmd is long like "G2 X23.32342 Y23.535355 Z1.04345 I0.243242 J-0.232455"
			// So we'll have to wait until the next time in here for this test to pass
			if b.q.LenOfCmds() < b.BufferMax {

				//log.Printf("tinyg just completed a line of gcode and there is room in buffer so setPaused(false)\n")

				// if we are paused, tell us to unpause cuz we have clean buffer room now
				if b.GetPaused() {

					// we are paused, but we can't just go unpause ourself, because we may
					// be manually paused. this means we have to do a double-check here
					// and not just go unpausing ourself just cuz we think there's room in the buffer.
					// this is because we could have just sent a ! to the tinyg. we may still
					// get back some random r:{} after the ! was sent, and that would mean we think
					// we can go sending more data, but really we can't cuz we were HARD Manually paused
					if b.GetManualPaused() == false {

						// we are not in a manual pause state, that means we can go ahead
						// and unpause ourselves
						b.SetPaused(false, 1) //set paused to false first, then release the hold on the buffer
					} else {
						log.Println("We just got incoming data so we could unpause, but since manual paused we will ignore until next time data comes in to unpause")
					}
				}
			}
			//b.lock.Unlock()
			//log.Printf("Done locking inside OnIncomingData\n")
		} else if b.initline.MatchString(element) {
			//grbl init line received, clear anything from current buffer and unpause
			b.LocalBufferWipe(b.parent_serport)

			//unpause buffer but wipe the command in the queue as grbl has restarted.
			if b.GetPaused() {
				b.SetPaused(false, 2)
			}
			var matches = b.initline.FindStringSubmatch(element)
			

			b.version = matches[1] //save element in version
			
			//Check for report output, compare to last report output, if different return to client to update status; otherwise ignore status.
		} else if b.rpt.MatchString(element) {
			if element == b.LastStatus {
				log.Println("Grbl status has not changed, not reporting to client")
				continue //skip this element as the cnc position has not changed, and move on to the next element.
			}

			b.LastStatus = element //if we make it here something has changed with the status string and laststatus needs updating
		} else if b.buf.MatchString(element) {
			var bufMatches = b.buf.FindStringSubmatch(element)
			b.availableBufferSpace, _ = strconv.Atoi(bufMatches[1])

		}
		// handle communication back to client
		// for base serial data (this is not the cmd:"Write" or cmd:"Complete")
		m := DataPerLine{b.Port, element + "\n"}
		bm, err := json.Marshal(m)
		if err == nil {
			h.broadcastSys <- bm
		}

	} // for loop

	// now wipe the LatestData to only have the last line that we did not analyze
	// because we didn't know/think that was a full command yet
	b.LatestData = arrLines[len(arrLines)-1]

	// we are losing incoming serial data because of garbageCollection()
	// doing a "stop the world" and all this data queues up back on the
	// tinyg and we miss stuff coming in, which gets our serial counter off
	// and then causes stalling, so we're going to attempt to force garbageCollection
	// each time we get data so that we don't have pauses as long as we were having
	if *gcType == "max" {
		debug.FreeOSMemory()
	}

	//time.Sleep(3000 * time.Millisecond)
	//log.Printf("OnIncomingData() end.\n")
}

//local version of buffer wipe loop needed to handle pseudo clear buffer (%) without passing that value on to
func (b *BufferflowGrbl) LocalBufferWipe(p *serport) {
	log.Printf("Pseudo command received to wipe grbl buffer but *not* send on to grbl controller.")

	// consume all stuff queued
	func() {
		ctr := 0

		keepLooping := true
		for keepLooping {
			select {
			case d, ok := <-p.sendBuffered:
				log.Printf("Consuming sendBuffered queue. ok:%v, d:%v, id:%v\n", ok, string(d.data), string(d.id))
				ctr++

				p.itemsInBuffer--
				if ok == false {
					keepLooping = false
				}
			default:
				keepLooping = false
				log.Println("Hit default in select clause")
			}
		}
		log.Printf("Done consuming sendBuffered cmds. ctr:%v\n", ctr)
	}()

	b.ReleaseLock()

	// let user know we wiped queue
	log.Printf("itemsInBuffer:%v\n", p.itemsInBuffer)
	h.broadcastSys <- []byte("{\"Cmd\":\"WipedQueue\",\"QCnt\":" + strconv.Itoa(p.itemsInBuffer) + ",\"Port\":\"" + p.portConf.Name + "\"}")
}
