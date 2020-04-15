package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mitchellh/go-homedir"
	ini "gopkg.in/ini.v1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/polly"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

// go install -ldflags "-X main.build=`git rev-parse --short=7 HEAD`"
var build string
var osversion string = runtime.GOOS

func printVersionInfo() {
	fmt.Printf("say v2 (%s) %s\n", build, osversion)
}

func main() {
	var inputpath string
	var versionflag bool
	flag.StringVar(&inputpath, "f", "", "optional path to input file")
	flag.BoolVar(&versionflag, "v", false, "print version")
	flag.Parse()
	fmt.Printf("input: %q\n", inputpath)

	if versionflag {
		printVersionInfo()
		os.Exit(0)
	}

	if strings.HasPrefix(inputpath, "~") {
		inputpath = strings.TrimLeft(inputpath, "~/")
		home, err := homedir.Dir()
		if err != nil {
			log.Fatal(err)
		}
		inputpath = filepath.Join(home, inputpath)
	}
	name := "echo"
	if len(inputpath) > 0 {

		// filename without extension
		name = filepath.Base(inputpath)
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	speaker := NewPolly(name, 4)
	fmt.Printf("name: %q\n", name)
	var doc []string
	doc = append(doc, flag.Arg(0))

	if len(inputpath) > 0 {
		doc = parseFile(inputpath)
	}
	speaker.SayAll(doc)

	speaker.Close()

	for i := 0; ; i++ {
		filename := fmt.Sprintf("%s%04d.mp3", name, i)
		_, err := os.Stat(filename)
		if err != nil {
			break
		}
		cmd := exec.Command("/usr/bin/afplay", "-q", "1", filename)
		err = cmd.Run()
		if err != nil {
			log.Error(err)
		}
	}
}

type Polly struct {
	ch chan *intstr
	wg sync.WaitGroup
}

type intstr struct {
	i int
	s string
}

func loadCredentials() (*credentials.Credentials, error) {
	home, err := homedir.Dir()
	if err != nil {
		return nil, err
	}
	configpath := filepath.Join(home, ".aws", "config")
	f, err := os.Open(configpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, err := ini.Load(f)
	if err != nil {
		return nil, err
	}
	def := cfg.Section("default")
	k, err := def.GetKey("aws_access_key_id")
	if err != nil {
		return nil, err
	}
	id := k.String()

	k, err = def.GetKey("aws_secret_access_key")
	if err != nil {
		return nil, err
	}
	secret := k.String()

	creds := credentials.NewStaticCredentials(id, secret, "")
	_, err = creds.Get()
	if err == nil {
		return creds, nil
	}
	creds = credentials.NewEnvCredentials()
	_, err = creds.Get()
	if err != nil {
		return nil, err
	}

	return creds, nil
}

func NewPolly(name string, threads int) *Polly {
	// awsLog := aws.LoggerFunc(func(args ...interface{}) {
	// 	log.Println(args...)
	// })
	creds, err := loadCredentials()
	if err != nil {
		log.Fatal(err)
	}

	awsCfg := &aws.Config{
		Region: aws.String("us-east-1"),
		CredentialsChainVerboseErrors: aws.Bool(true),
		Credentials:                   creds,
		// Credentials:                   credentials.NewStaticCredentials("account id", "secret key", ""),
		// LogLevel:                      aws.LogLevel(aws.LogDebug | aws.LogDebugWithSigning | aws.LogDebugWithHTTPBody),
		// Logger:                        awsLog,
	}

	sess := session.New(awsCfg)
	svc := polly.New(sess)

	p := &Polly{ch: make(chan *intstr, 1)}

	var size int64
	p.wg.Add(threads)

	var off int
	for i := 0; i < threads; i++ {
		go func(i int) {
			defer p.wg.Done()
			input := &polly.SynthesizeSpeechInput{
				OutputFormat: aws.String("mp3"),
				SampleRate:   aws.String("22050"),
				TextType:     aws.String("ssml"),
				VoiceId:      aws.String("Brian"),
			}

			for is := range p.ch {
				if len(is.s) == 0 {
					continue
				}

				phrase := "<speak><prosody rate='1.3'>" + is.s + "</prosody></speak>"
				input.Text = &phrase

				result, err := svc.SynthesizeSpeech(input)
				if err != nil {
					log.Errorf("Error %s\n%s\n", err, is.s)
					sentences := strings.Split(is.s, ".")
					for _, s := range sentences {
						phrase := "<speak><prosody rate='1.3'>" + s + ".</prosody></speak>"
						input.Text = &phrase
						result, err = svc.SynthesizeSpeech(input)
						if err != nil {
							log.Fatalf("Error %s\n%s\n", err, s)
						}
						writeAudio(name, &size, is.i+off, result.AudioStream)
						off++
						result.AudioStream.Close()
					}
					continue
				}

				writeAudio(name, &size, is.i+off, result.AudioStream)
				result.AudioStream.Close()

			}

		}(i)
	}

	return p
}

func writeAudio(name string, size *int64, i int, stream io.Reader) {
	data, err := ioutil.ReadAll(stream)
	if err != nil {
		log.Error(err)
		return
	}

	filename := fmt.Sprintf("%s%04d.mp3", name, i)
	err = ioutil.WriteFile(filename, data, 0644)
	if err != nil {
		log.Error(err)
	}

	atomic.AddInt64(size, int64(len(data)))
	// log.Infof("%s: %s / %s", filename, humanize.Bytes(uint64(len(data))), humanize.Bytes(uint64(*size)))
}

func (p *Polly) Close() error {
	close(p.ch)
	p.wg.Wait()
	return nil
}

func (p Polly) SayAll(phrases []string) {
	for i, phrase := range phrases {
		p.ch <- &intstr{i, phrase}
	}
}

func pollyError(err error) {
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case polly.ErrCodeTextLengthExceededException:
				log.Errorln(polly.ErrCodeTextLengthExceededException, aerr.Error())
			case polly.ErrCodeInvalidSampleRateException:
				log.Errorln(polly.ErrCodeInvalidSampleRateException, aerr.Error())
			case polly.ErrCodeInvalidSsmlException:
				log.Errorln(polly.ErrCodeInvalidSsmlException, aerr.Error())
			case polly.ErrCodeLexiconNotFoundException:
				log.Errorln(polly.ErrCodeLexiconNotFoundException, aerr.Error())
			case polly.ErrCodeServiceFailureException:
				log.Errorln(polly.ErrCodeServiceFailureException, aerr.Error())
			case polly.ErrCodeMarksNotSupportedForFormatException:
				log.Errorln(polly.ErrCodeMarksNotSupportedForFormatException, aerr.Error())
			case polly.ErrCodeSsmlMarksNotSupportedForTextTypeException:
				log.Errorln(polly.ErrCodeSsmlMarksNotSupportedForTextTypeException, aerr.Error())
			default:
				log.Errorln(aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			log.Errorln(err.Error())
		}
		return
	}

}
func parseFile(filename string) []string {

	abs, err := filepath.Abs(filename)
	if err != nil {
		log.Fatalf("error opening %q: %s", filename, err)
	}

	f, err := os.Open(abs)
	if err != nil {
		log.Fatalf("error opening %q: %s", abs, err)
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("finished reading %q", filename)
	var runeData []rune
	for _, r := range []rune(string(data)) {
		switch r {
		case '<':
			runeData = append(runeData, []rune("&lt;")...)
		case '>':
			runeData = append(runeData, []rune("&gt;")...)
		case '&':
			runeData = append(runeData, []rune("&amp;")...)
		case '"':
			runeData = append(runeData, []rune("&quot;")...)
		case '\'':
			runeData = append(runeData, []rune("&apos;")...)
		case '¢':
			runeData = append(runeData, []rune("&cent;")...)
		case '£':
			runeData = append(runeData, []rune("&pound;")...)
		case '¥':
			runeData = append(runeData, []rune("&yen;")...)
		case '€':
			runeData = append(runeData, []rune("&euro;")...)
		case '©':
			runeData = append(runeData, []rune("&copy;")...)
		case '®':
			runeData = append(runeData, []rune("&reg;")...)
		default:
			runeData = append(runeData, r)
		}
	}

	paragraphs := strings.Split(string(runeData), "\n")
	var blocks []string
	var block string

	// for each newline
	for i := 0; i < len(paragraphs); i++ {
		// if it is empty, continue
		if len(paragraphs[i]) == 0 {
			continue
		}

		// if added to the previous block it is still less then the polly character
		// limit, add it to the block and continue
		if len(block)+len(paragraphs[i])+1 < 1500 {
			block += paragraphs[i] + "\n"
			continue
		}
		// if it is too big, save the existing block, and create a new empty one.
		blocks = append(blocks, block)
		block = ""

		// if it fits now, add it to the new block and continue
		if len(block)+len(paragraphs[i])+1 < 1500 {
			block = paragraphs[i] + "\n"
			continue
		}

		// if it still doesn't fit, break it down to sentences.
		for _, sentence := range strings.Split(paragraphs[i], ".") {
			if len(block)+len(sentence)+1 < 1500 {
				block = sentence + "."
				continue
			}

			// if it is too big, save the existing block, and create a new empty one.
			blocks = append(blocks, block)
			block = ""

			if len(block)+len(sentence)+1 < 1500 {
				block = sentence + "."
				continue
			}
			log.Fatalf("sentence is too long for a single request %q", sentence)
		}
	}
	return append(blocks, block)
}

// notes on direct playback via portaudio

/*
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"

	"github.com/bobertlo/go-mpg123/mpg123"
	"github.com/gordonklaus/portaudio"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("missing required argument:  input file name")
		return
	}
	fmt.Println("Playing.  Press Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	// create mpg123 decoder instance
	decoder, err := mpg123.NewDecoder("")
	chk(err)

	fileName := os.Args[1]
	chk(decoder.Open(fileName))
	defer decoder.Close()

	// get audio format information
	rate, channels, _ := decoder.GetFormat()

	// make sure output format does not change
	decoder.FormatNone()
	decoder.Format(rate, channels, mpg123.ENC_SIGNED_16)

	portaudio.Initialize()
	defer portaudio.Terminate()
	out := make([]int16, 8192)
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(rate), len(out), &out)
	chk(err)
	defer stream.Close()

	chk(stream.Start())
	defer stream.Stop()
	for {
		audio := make([]byte, 2*len(out))
		_, err = decoder.Read(audio)
		if err == mpg123.EOF {
			break
		}
		chk(err)

		chk(binary.Read(bytes.NewBuffer(audio), binary.LittleEndian, out))
		chk(stream.Write())
		select {
		case <-sig:
			return
		default:
		}
	}
}

func chk(err error) {
	if err != nil {
		panic(err)
	}
}
*/
