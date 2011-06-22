// scores.go
//
// Score handling
//
// In here, we have the probing code that learns about scores, reads
// their configuration files, and does the heavy lifting for launching
// them, doing the privilege drop, etc.

package main

import (
	"os"
	"io"
	"strings"
	o "orchestra"
	"path"
	"bufio"
	"unicode"
)

const (
	SI_FIFO		= iota
	SI_ENV
	// Score Flags
	SF_SETUID	= 1<<0
	SF_SETGID	= 1<<1
)

type ScoreInfo struct {
	Name		string
	Executable	string

	Interface	int
}

func NewScoreInfo() (si *ScoreInfo) {
	si = new (ScoreInfo)
	// establish defaults
	si.Interface = SI_ENV

	return si
}

var (
	Scores		map[string]*ScoreInfo
)

func ScoreConfigure(si *ScoreInfo, r io.Reader) {
	br := bufio.NewReader(r)

	linenum := 1

	for {
		var linebytes []byte = nil
		var part bool
		var bytes []byte = nil
		var err os.Error
		
		bytes, part, err = br.ReadLine()
		if err != nil && err != os.EOF {
			o.Fail("Error reading configuration: %s", err)
		}
		if err == os.EOF {
			break;
		}
		linebytes = append(linebytes,bytes...)
		if err != os.EOF {
			for ; part; bytes,part,err = br.ReadLine() {
				if err != nil  {
					break
				}
				linebytes = append(linebytes, bytes...)
			}
			if err != nil && err != os.EOF {
				o.Fail("Error reading configuration: %s", err)
			}
		}
		line := string(linebytes)
		// prune leading whitespace.
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		// skip comments
		if strings.HasPrefix(line, "#") {
			continue;
		}
		// split into fields
		bits := strings.Fields(line)
		if len(bits) == 0 {
			continue;
		}
		switch bits[0] {
		case "interface":
			if len(bits) != 2 {
				o.Fail("Malformed score configuration on line %d: too many arguments to interface", linenum)
			}
			switch bits[1] {
			case "env":
				si.Interface = SI_ENV
			case "fifo":
				si.Interface = SI_FIFO
			default:
				o.Fail("Malformed score configuration on line %d: Unknown interface type %s", linenum, bits[1])
			}
		default:
			o.Fail("Unknown configuration directive %s on line %d", bits[0], linenum)
		}
		linenum++
	}	

}

func LoadScores() {
	dir, err := os.Open(*ScoreDirectory)
	o.MightFail("Couldn't open Score directory", err)

	Scores = make(map[string]*ScoreInfo)
	
	files, err := dir.Readdir(-1)
	for i := range files {
		// skip ., .. and other dotfiles.
		if strings.HasPrefix(files[i].Name, ".") {
			continue
		}
		// emacs backup files.  ignore these.
		if strings.HasSuffix(files[i].Name, "~") || strings.HasPrefix(files[i].Name, "#") {
			continue
		}
		// .conf is reserved for score configurations.
		if strings.HasSuffix(files[i].Name, ".conf") {
			continue
		}
		if !files[i].IsRegular() && !files[i].IsSymlink() {
			continue
		}

		//FIXME: we should check for the execution flag, but I'm lazy
		fullpath := path.Join(*ScoreDirectory, files[i].Name)
		conffile := fullpath+".conf"
		o.Warn("Considering %s as score", files[i].Name)

		si := NewScoreInfo()
		si.Name = files[i].Name
		si.Executable = fullpath
		
		conf, err := os.Open(conffile)
		if err == nil {
			o.Warn("Parsing configuration for %s", fullpath)
			ScoreConfigure(si, conf)
			conf.Close()
		} else {
			o.Warn("Couldn't open config file for %s, assuming defaults: %s", files[i].Name, err)
		}
		Scores[files[i].Name] = si
	}
}