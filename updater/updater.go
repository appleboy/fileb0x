package updater

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"strings"

	"encoding/hex"

	"encoding/json"

	"github.com/appleboy/fileb0x/file"
	"github.com/appleboy/mpb"
	"github.com/appleboy/mpb/decor"
)

var p *mpb.Progress

// Auth holds authentication for the http basic auth
type Auth struct {
	Username string
	Password string
}

// ResponseInit holds a list of hashes from the server
// to be sent to the client so it can check if there
// is a new file or a changed file
type ResponseInit struct {
	Success bool
	Hashes  map[string]string
}

// Updater sends files that should be update to the b0x server
type Updater struct {
	Server string
	Auth   Auth

	RemoteHashes map[string]string
	LocalHashes  map[string]string
	ToUpdate     []string
	Workers      int
}

// Init gets the list of file hash from the server
func (up *Updater) Init() error {
	return up.Get()
}

// Get gets the list of file hash from the server
func (up *Updater) Get() error {
	log.Println("Creating hash list request...")
	req, err := http.NewRequest("GET", up.Server, nil)
	if err != nil {
		return err
	}

	req.SetBasicAuth(up.Auth.Username, up.Auth.Password)

	log.Println("Sending hash list request...")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("Error Unautorized")
	}

	log.Println("Reading hash list response's body...")
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	if err != nil {
		return err
	}

	log.Println("Parsing hash list response's body...")
	ri := &ResponseInit{}
	err = json.Unmarshal(buf.Bytes(), &ri)
	if err != nil {
		log.Println("Body is", buf.Bytes())
		return err
	}
	resp.Body.Close()

	// copy hash list
	if ri.Success {
		log.Println("Copying hash list...")
		up.RemoteHashes = ri.Hashes
		up.LocalHashes = map[string]string{}
		log.Println("Done")
	}

	return nil
}

// Updatable checks if there is any file that should be updaTed
func (up *Updater) Updatable(files map[string]*file.File) (bool, error) {
	hasUpdates := !up.EqualHashes(files)

	if hasUpdates {
		log.Println("----------------------------------------")
		log.Println("-- Found files that should be updated --")
		log.Println("----------------------------------------")
	} else {
		log.Println("-----------------------")
		log.Println("-- Nothing to update --")
		log.Println("-----------------------")
	}

	return hasUpdates, nil
}

// EqualHash checks if a local file hash equals a remote file hash
// it returns false when a remote file hash isn't found (new files)
func (up *Updater) EqualHash(name string) bool {
	hash, existsLocally := up.LocalHashes[name]
	_, existsRemotely := up.RemoteHashes[name]
	if !existsRemotely || !existsLocally || hash != up.RemoteHashes[name] {
		if hash != up.RemoteHashes[name] {
			log.Println("Found changes in file: ", name)

		} else if !existsRemotely && existsLocally {
			log.Println("Found new file: ", name)
		}

		return false
	}

	return true
}

// EqualHashes builds the list of local hashes before
// checking if there is any that should be updated
func (up *Updater) EqualHashes(files map[string]*file.File) bool {
	for _, f := range files {
		log.Println("Checking file for changes:", f.Path)

		if len(f.Bytes) == 0 && !f.ReplacedText {
			data, err := ioutil.ReadFile(f.OriginalPath)
			if err != nil {
				log.Fatal(err)
			}

			f.Bytes = data

			// removes the []byte("") from the string
			// when the data isn't in the Bytes variable
		} else if len(f.Bytes) == 0 && f.ReplacedText && len(f.Data) > 0 {
			f.Data = strings.TrimPrefix(f.Data, `[]byte("`)
			f.Data = strings.TrimSuffix(f.Data, `")`)
			f.Data = strings.Replace(f.Data, "\\x", "", -1)

			var err error
			f.Bytes, err = hex.DecodeString(f.Data)
			if err != nil {
				log.Println("SHIT", err)
				return false
			}

			f.Data = ""
		}

		sha := sha256.New()
		if _, err := sha.Write(f.Bytes); err != nil {
			log.Fatal(err)
			return false
		}

		up.LocalHashes[f.Path] = hex.EncodeToString(sha.Sum(nil))
	}

	// check if there is any file to update
	update := false
	for k := range up.LocalHashes {
		if !up.EqualHash(k) {
			up.ToUpdate = append(up.ToUpdate, k)
			update = true
		}
	}

	return !update
}

type job struct {
	current int
	files   *file.File
	total   int
}

// UpdateFiles sends all files that should be updated to the server
// the limit is 3 concurrent files at once
func (up *Updater) UpdateFiles(files map[string]*file.File) error {
	updatable, err := up.Updatable(files)
	if err != nil {
		return err
	}

	if !updatable {
		return nil
	}

	// progressbar pool
	p = mpb.New(mpb.WithWidth(100))

	total := len(up.ToUpdate)
	jobs := make(chan *job, total)
	done := make(chan bool, total)

	if up.Workers <= 0 {
		up.Workers = 1
	}

	for i := 0; i < up.Workers; i++ {
		go up.worker(jobs, done)
	}

	for i, name := range up.ToUpdate {
		jobs <- &job{
			current: i + 1,
			files:   files[name],
			total:   total,
		}
	}
	close(jobs)

	for i := 0; i < total; i++ {
		<-done
	}

	p.Wait()
	return nil
}

func (up *Updater) worker(jobs <-chan *job, done chan<- bool) {
	for job := range jobs {
		f := job.files
		// log.Println("RUNNING JOB", fmt.Sprintf("%d/%d %s", job.current, job.total, f.Path))

		fr := bytes.NewReader(f.Bytes)

		jobText := fmt.Sprintf("%d/%d | ", job.current, job.total)
		nameText := fmt.Sprintf("%s | ", f.Path)
		bar := p.AddBar(fr.Size(),
			mpb.PrependDecorators(
				decor.StaticName(jobText, 0, 0),
				decor.StaticName(nameText, 0, decor.DSyncSpace),
				decor.CountersKibiByte("%6.1f / %6.1f", 18, decor.DSyncSpace),
			),
			mpb.AppendDecorators(decor.ETA(5, decor.DwidthSync)),
		)

		p.UpdateBarPriority(bar, job.current)

		r, w := io.Pipe()
		writer := multipart.NewWriter(w)

		// copy the file into the form
		go func(fr io.Reader) {
			defer w.Close()
			part, err := writer.CreateFormFile("file", f.Path)
			if err != nil {
				log.Fatal(err)
			}

			pr := bar.ProxyReader(fr)

			_, err = io.Copy(part, pr)
			if err != nil {
				log.Fatal(err)
			}

			err = writer.Close()
			if err != nil {
				log.Fatal(err)
			}
		}(fr)

		// create a post request with basic auth
		// and the file included in a form
		req, err := http.NewRequest("POST", up.Server, r)
		if err != nil {
			log.Fatal(err)
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.SetBasicAuth(up.Auth.Username, up.Auth.Password)

		// sends the request
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}

		body := &bytes.Buffer{}
		_, err = body.ReadFrom(resp.Body)
		if err != nil {
			log.Fatal(err)
		}

		if err := resp.Body.Close(); err != nil {
			log.Fatal(err)
		}

		if body.String() != "ok" {
			log.Fatal(body.String())
		}

		bar.Complete()
		done <- true
	}
}
