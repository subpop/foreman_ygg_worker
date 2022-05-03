package main

import (
	"context"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"git.sr.ht/~spc/go-log"
	pb "github.com/redhatinsights/yggdrasil/protocol"
)

func dispatch(ctx context.Context, d *pb.Data, s *jobStorage) {
	event, prs := d.GetMetadata()["event"]
	if !prs {
		log.Warnln("Message metadata does not contain event field, assuming 'start'")
		event = "start"
	}

	switch event {
	case "start":
		startScript(ctx, d, s)
	case "cancel":
		cancel(ctx, d, s)
	default:
		log.Errorf("Received unknown event '%v'", event)
	}
}

func startScript(ctx context.Context, d *pb.Data, s *jobStorage) {
	jobUUID, jobUUIDP := d.GetMetadata()["job_uuid"]
	if !jobUUIDP {
		log.Warnln("No job uuid found in job's metadata, will not be able to cancel this job")
	}

	script := string(d.GetContent())
	log.Tracef("running script : %#v", script)

	scriptfile, err := ioutil.TempFile("/tmp", "ygg_rex")
	if err != nil {
		log.Errorf("failed to create script tmp file: %v", err)
	}
	defer os.Remove(scriptfile.Name())

	n2, err := scriptfile.Write(d.GetContent())
	if err != nil {
		log.Errorf("failed to write script to tmp file: %v", err)
	}
	log.Debugf("script of %d bytes written in : %#v", n2, scriptfile.Name())

	err = scriptfile.Close()
	if err != nil {
		log.Fatal(err)
	}

	err = os.Chmod(scriptfile.Name(), 0700)
	if err != nil {
		log.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", "-c", scriptfile.Name())
	// cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Errorf("cannot connect to stdout: %v", err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Errorf("cannot connect to stderr: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Errorf("cannot run script: %v", err)
		return
	}
	log.Infof("started script process: %v", cmd.Process.Pid)
	if jobUUIDP {
		s.Set(jobUUID, cmd.Process.Pid)
		defer s.Remove(jobUUID)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	updates := make(chan V1Update)

	oa := NewUpdateAggregator(d.GetMetadata()["return_url"], d.GetMessageId())
	go oa.Aggregate(updates, &YggdrasilGrpc{})

	go func() { outputCollector("stdout", stdout, updates); wg.Done() }()
	go func() { outputCollector("stderr", stderr, updates); wg.Done() }()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// The program has exited with an exit code != 0
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				updates <- NewExitUpdate(status.ExitStatus())
			}
		} else {
			log.Errorf("script run failed: %v", err)
		}
	} else {
		updates <- NewExitUpdate(0)
	}
	close(updates)
}

func outputCollector(stdtype string, pipe io.ReadCloser, outputs chan<- V1Update) {
	buf := make([]byte, 4096)
	for {
		n, err := pipe.Read(buf)
		if n > 0 {
			msg := string(buf[:n])
			log.Tracef("%v message: %v", stdtype, msg)
			outputs <- NewOutputUpdate(stdtype, msg)
		}
		if err != nil {
			if err != io.EOF {
				log.Errorf("cannot read from %v: %v", stdtype, err)
			}
			break
		}
	}
}

var syscallKill = syscall.Kill

func cancel(ctx context.Context, d *pb.Data, s *jobStorage) {
	jobUUID, jobUUIDP := d.GetMetadata()["job_uuid"]
	if !jobUUIDP {
		log.Errorln("No job uuid found in job's metadata, aborting.")
		return
	}

	pid, prs := s.Get(jobUUID)
	if !prs {
		log.Errorf("Cannot cancel unknown job %v", jobUUID)
		return
	}

	log.Infof("Cancelling job %v, sending SIGTERM to process %v", jobUUID, pid)
	if err := syscallKill(pid, syscall.SIGTERM); err != nil {
		log.Errorf("Failed to send SIGTERM to process %v: %v", pid, err)
	}
}
