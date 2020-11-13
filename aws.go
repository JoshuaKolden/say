package main

import (
	"bytes"
	"io"
	"io/ioutil"
	"path/filepath"

	"github.com/mitchellh/go-homedir"
	ini "gopkg.in/ini.v1"

	"github.com/aws/aws-sdk-go/aws/credentials"
)

// LoadAWSCredentials - Looks in the user's `.aws` path for the configuration
// and loads the credentials for `identity`. If `identity` is empty then
// the `default` identity is loaded. If it fails to find credentials in
// the aws config it attempts to find credentials in the environment variables.
func LoadAWSCredentials(identity string) (*credentials.Credentials, error) {
	home, err := homedir.Dir()
	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadFile(filepath.Join(home, ".aws", "config"))
	if err != nil {
		return nil, err
	}

	id, secret, err := iniLoadIdentity(bytes.NewReader(data), identity)
	if err != nil {
		return nil, err
	}

	creds := credentials.NewStaticCredentials(id, secret, "")
	_, err = creds.Get()
	if err != nil {
		creds = credentials.NewEnvCredentials()
		_, err := creds.Get()
		if err != nil {
			return nil, err
		}
	}

	return creds, err
}

// iniLoadIdentity - parses AWS credentials from the ini format config file
// `identity` specifies the identity to load, if empty the "default" is loaded
//
// The return values are the `access_key_id`, the `secret_key` and an error.
func iniLoadIdentity(r io.Reader, identity string) (string, string, error) {
	if len(identity) == 0 {
		identity = "default"
	}

	cfg, err := ini.Load(r)
	if err != nil {
		return "", "", err
	}
	def := cfg.Section(identity)

	creds := []string{"aws_access_key_id", "aws_secret_access_key"}
	for i, label := range creds {
		k, err := def.GetKey(label)
		if err != nil {
			return "", "", err
		}
		creds[i] = k.String()
	}

	return creds[0], creds[1], nil
}
