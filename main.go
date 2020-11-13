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

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/polly"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

// go install -ldflags "-X main.build=`git rev-parse --short=7 HEAD`"
var build string
var osversion string = runtime.GOOS

func main() {
	var inputpath string
	var showVersion, polyFlag bool

	flag.StringVar(&inputpath, "f", "", "optional path to text input")
	flag.BoolVar(&polyFlag, "p", true, "skip poly, and play audio (if all mp3s exist)")
	flag.BoolVar(&showVersion, "v", false, "print version")
	flag.Parse()

	if showVersion {
		fmt.Printf("say v3 (%s) %s\n", build, osversion)
		os.Exit(0)
	}

	if len(inputpath) != 0 {
		fmt.Printf("input: %q\n", inputpath)
	}

	inputpath, err := TildePath(inputpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	name := "echo"
	if len(inputpath) > 0 {
		// filename without extension
		name = filepath.Base(inputpath)
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}

	speaker := NewPolly(4)

	doc := ReadTextInput(inputpath)
	if len(inputpath) == 0 {
		doc = append(doc, flag.Arg(0))
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
			// log.Error(err)
			fmt.Fprintf(os.Stderr, "%s\n", err)
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

/*
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
*/

func NewAWSSession(identity, region string) (*session.Session, error) {
	creds, err := LoadAWSCredentials(identity)
	if err != nil {
		return nil, err
	}

	awsCfg := &aws.Config{
		Region:                        &region,
		CredentialsChainVerboseErrors: aws.Bool(true),
		Credentials:                   creds,
	}

	return session.New(awsCfg), nil
}

func NewPolly(threads int) *Polly {
	sess, err := NewAWSSession("default", "us-east-1")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(2)
	}

	svc := polly.New(sess)

	polyInput := polly.SynthesizeSpeechInput{
		OutputFormat: aws.String("mp3"),
		SampleRate:   aws.String("22050"),
		TextType:     aws.String("ssml"),
		VoiceId:      aws.String("Brian"),
	}
	printError := func(err error, quote string) {
		if err != nil {
			return
		}

		if len(quote) == 0 {
			fmt.Fprintf(os.Stderr, "Error %s\n", err)
			return
		}

		fmt.Fprintf(os.Stderr, "Error %s\n%s\n", err, quote)
	}

	var size int64
	var off int

	p := &Polly{ch: make(chan *intstr, 1)}
	p.wg.Add(threads)
	for i := 0; i < threads; i++ {
		go func(i int) {
			defer p.wg.Done()

			for is := range p.ch {
				section := is.s
				if len(section) == 0 {
					continue
				}
				input := polyInput

				phrase := "<speak><prosody rate='1.3'>" + section + "</prosody></speak>"

				input.Text = &phrase
				rsp, err := svc.SynthesizeSpeech(&input)
				if err != nil {
					printError(err, section)
					// fmt.Fprintf(os.Stderr, errfmt, err, section)
					// Sub-devide the input by sentince, and try again.
					sentences := strings.Split(section, ".")
					for _, sentence := range sentences {

						input.Text = &phrase
						rsp, err = svc.SynthesizeSpeech(&input)
						if err != nil {
							// fmt.Fprintf(os.Stderr, errfmt, err, s)
							printError(err, sentence)
							continue
						}

						n, err := WriteMP3(fmt.Sprintf("%04d.mp3", is.i+off), rsp.AudioStream)
						if err != nil {
							// fmt.Fprintf(os.Stderr, errfmt, err, section)
							printError(err, sentence)
							continue
						}
						atomic.AddInt64(&size, n)
						off++
						rsp.AudioStream.Close()
					}
					continue
				}

				n, err := WriteMP3(fmt.Sprintf("%04d.mp3", is.i+off), rsp.AudioStream)
				if err != nil {
					// fmt.Fprintf(os.Stderr, errfmt, err, s)
					printError(err, section)
					os.Exit(5)
				}
				atomic.AddInt64(&size, n)
				rsp.AudioStream.Close()
			}
		}(i)
	}
	return p
}

func WriteMP3(filename string, fin io.Reader) (int64, error) {
	fout, err := os.Create(filename)
	if err != nil {
		return 0, err
	}
	defer fout.Close()

	return io.Copy(fout, fin)
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
	if err == nil {
		return
	}
	// Print the error, cast err to awserr.Error to get the Code and message from
	// an error.
	aerr, ok := err.(awserr.Error)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		return
	}

	switch aerr.Code() {
	case polly.ErrCodeTextLengthExceededException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeTextLengthExceededException, aerr.Error())
	case polly.ErrCodeInvalidSampleRateException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeInvalidSampleRateException, aerr.Error())
	case polly.ErrCodeInvalidSsmlException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeInvalidSsmlException, aerr.Error())
	case polly.ErrCodeLexiconNotFoundException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeLexiconNotFoundException, aerr.Error())
	case polly.ErrCodeServiceFailureException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeServiceFailureException, aerr.Error())
	case polly.ErrCodeMarksNotSupportedForFormatException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeMarksNotSupportedForFormatException, aerr.Error())
	case polly.ErrCodeSsmlMarksNotSupportedForTextTypeException:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", polly.ErrCodeSsmlMarksNotSupportedForTextTypeException, aerr.Error())
	default:
		fmt.Fprintf(os.Stderr, "AWS Poly error #%d: %s\n", aerr.Error())
	}
}

func ReadTextInput(filename string) []string {
	var out []string
	if len(filename) == 0 {
		return out
	}

	// path, err := filepath.Abs(filename)
	// if err != nil {
	// 	fmt.Fprintf(os.Stderr, "error opening %q: %s", filename, err)
	// 	return out
	// }
	data, err := ioutil.ReadFile(filename)
	// f, err := os.Open(path)
	// if err != nil {
	// 	fmt.Fprintf(os.Stderr, "error opening %q: %s", filename, err)
	// 	return out
	// }
	// defer f.Close()

	// data, err := ioutil.ReadAll(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s %s\n", filename, err)
		return out
	}
	// log.Infof("finished reading %q", filename)
	var runeData []rune
	for _, r := range []rune(string(data)) {
		switch r {
		case '#':
			runeData = append(runeData, []rune("&hash;")...)
		case '<':
			runeData = append(runeData, []rune("&lt;")...)
		case '>':
			runeData = append(runeData, []rune("&gt;")...)
		case '&':
			runeData = append(runeData, []rune("&amp;")...)
		case '"':
		case '“':
		case '”':
			runeData = append(runeData, []rune("&quot;")...)
		case '\'':
		case '’':
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
		out = append(out, block)
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
			out = append(out, block)
			block = ""

			if len(block)+len(sentence)+1 < 1500 {
				block = sentence + "."
				continue
			}
			fmt.Fprintf(os.Stderr, "sentence is too long for a single request %q\n", sentence)
		}
	}

	return append(out, block)
}

func TildePath(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}

	home, err := homedir.Dir()
	if err != nil {
		return path, err
	}

	return filepath.Join(home, strings.TrimLeft(path, "~/")), nil
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
