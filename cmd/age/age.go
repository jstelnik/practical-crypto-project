// Copyright 2019 The age Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	age "github.com/srest2021/practical-crypto-project"
	"github.com/srest2021/practical-crypto-project/agessh"
	"github.com/srest2021/practical-crypto-project/armor"
	"github.com/srest2021/practical-crypto-project/plugin"
	"golang.org/x/term"
)

const usage = `Usage:
    age [--encrypt] (-r RECIPIENT | -R PATH)... [--armor] [-o OUTPUT] [INPUT]
    age [--encrypt] --passphrase [--armor] [-o OUTPUT] [INPUT]
    age --decrypt [-i PATH]... [-o OUTPUT] [INPUT]

Options:
    -e, --encrypt               Encrypt the input to the output. Default if omitted.
    -d, --decrypt               Decrypt the input to the output.
    -o, --output OUTPUT         Write the result to the file at path OUTPUT.
    -a, --armor                 Encrypt to a PEM encoded format.
    -p, --passphrase            Encrypt with a passphrase.
    -r, --recipient RECIPIENT   Encrypt to the specified RECIPIENT. Can be repeated.
    -R, --recipients-file PATH  Encrypt to recipients listed at PATH. Can be repeated.
    -i, --identity PATH         Use the identity file at PATH. Can be repeated.

INPUT defaults to standard input, and OUTPUT defaults to standard output.
If OUTPUT exists, it will be overwritten.

RECIPIENT can be an age public key generated by age-keygen ("age1...")
or an SSH public key ("ssh-ed25519 AAAA...", "ssh-rsa AAAA...").

Recipient files contain one or more recipients, one per line. Empty lines
and lines starting with "#" are ignored as comments. "-" may be used to
read recipients from standard input.

Identity files contain one or more secret keys ("AGE-SECRET-KEY-1..."),
one per line, or an SSH key. Empty lines and lines starting with "#" are
ignored as comments. Passphrase encrypted age files can be used as
identity files. Multiple key files can be provided, and any unused ones
will be ignored. "-" may be used to read identities from standard input.

When --encrypt is specified explicitly, -i can also be used to encrypt to an
identity file symmetrically, instead or in addition to normal recipients.

Example:
    $ age-keygen -o key.txt
    Public key: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
    $ tar cvz ~/data | age -r age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p > data.tar.gz.age
    $ age --decrypt -i key.txt -o data.tar.gz data.tar.gz.age`

// Version can be set at link time to override debug.BuildInfo.Main.Version,
// which is "(devel)" when building from within the module. See
// golang.org/issue/29814 and golang.org/issue/29228.
var Version string

// stdinInUse is used to ensure only one of input, recipients, or identities
// file is read from stdin. It's a singleton like os.Stdin.
var stdinInUse bool

type multiFlag []string

func (f *multiFlag) String() string { return fmt.Sprint(*f) }

func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type identityFlag struct {
	Type, Value string
}

// identityFlags tracks -i and -j flags, preserving their relative order, so
// that "age -d -j agent -i encrypted-fallback-keys.age" behaves as expected.
type identityFlags []identityFlag

func (f *identityFlags) addIdentityFlag(value string) error {
	*f = append(*f, identityFlag{Type: "i", Value: value})
	return nil
}

func (f *identityFlags) addPluginFlag(value string) error {
	*f = append(*f, identityFlag{Type: "j", Value: value})
	return nil
}

func main() {
	flag.Usage = func() { fmt.Fprintf(os.Stderr, "%s\n", usage) }

	if len(os.Args) == 1 {
		flag.Usage()
		exit(1)
	}

	var (
		outFlag                          string
		decryptFlag, encryptFlag         bool
		passFlag, versionFlag, armorFlag bool
		recipientFlags                   multiFlag
		recipientsFileFlags              multiFlag
		identityFlags                    identityFlags
	)

	flag.BoolVar(&versionFlag, "version", false, "print the version")
	flag.BoolVar(&decryptFlag, "d", false, "decrypt the input")
	flag.BoolVar(&decryptFlag, "decrypt", false, "decrypt the input")
	flag.BoolVar(&encryptFlag, "e", false, "encrypt the input")
	flag.BoolVar(&encryptFlag, "encrypt", false, "encrypt the input")
	flag.BoolVar(&passFlag, "p", false, "use a passphrase")
	flag.BoolVar(&passFlag, "passphrase", false, "use a passphrase")
	flag.StringVar(&outFlag, "o", "", "output to `FILE` (default stdout)")
	flag.StringVar(&outFlag, "output", "", "output to `FILE` (default stdout)")
	flag.BoolVar(&armorFlag, "a", false, "generate an armored file")
	flag.BoolVar(&armorFlag, "armor", false, "generate an armored file")
	flag.Var(&recipientFlags, "r", "recipient (can be repeated)")
	flag.Var(&recipientFlags, "recipient", "recipient (can be repeated)")
	flag.Var(&recipientsFileFlags, "R", "recipients file (can be repeated)")
	flag.Var(&recipientsFileFlags, "recipients-file", "recipients file (can be repeated)")
	flag.Func("i", "identity (can be repeated)", identityFlags.addIdentityFlag)
	flag.Func("identity", "identity (can be repeated)", identityFlags.addIdentityFlag)
	flag.Func("j", "data-less plugin (can be repeated)", identityFlags.addPluginFlag)
	flag.Parse()

	if versionFlag {
		if Version != "" {
			fmt.Println(Version)
			return
		}
		if buildInfo, ok := debug.ReadBuildInfo(); ok {
			// TODO: use buildInfo.Settings to prepare a pseudoversion such as
			// v0.0.0-20210817164053-32db794688a5+dirty on Go 1.18+.
			fmt.Println(buildInfo.Main.Version)
			return
		}
		fmt.Println("(unknown)")
		return
	}

	if flag.NArg() > 1 {
		var hints []string
		quotedArgs := strings.Trim(fmt.Sprintf("%q", flag.Args()), "[]")

		// If the second argument looks like a flag, suggest moving the first
		// argument to the back (as long as the arguments don't need quoting).
		if strings.HasPrefix(flag.Arg(1), "-") {
			hints = append(hints, "the input file must be specified after all flags")

			safe := true
			unsafeShell := regexp.MustCompile(`[^\w@%+=:,./-]`)
			for _, arg := range os.Args {
				if unsafeShell.MatchString(arg) {
					safe = false
					break
				}
			}
			if safe {
				i := len(os.Args) - flag.NArg()
				newArgs := append([]string{}, os.Args[:i]...)
				newArgs = append(newArgs, os.Args[i+1:]...)
				newArgs = append(newArgs, os.Args[i])
				hints = append(hints, "did you mean:")
				hints = append(hints, "    "+strings.Join(newArgs, " "))
			}
		} else {
			hints = append(hints, "only a single input file may be specified at a time")
		}

		errorWithHint("too many INPUT arguments: "+quotedArgs, hints...)
	}

	switch {
	case decryptFlag:
		if encryptFlag {
			errorf("-e/--encrypt can't be used with -d/--decrypt")
		}
		if armorFlag {
			errorWithHint("-a/--armor can't be used with -d/--decrypt",
				"note that armored files are detected automatically")
		}
		if passFlag {
			errorWithHint("-p/--passphrase can't be used with -d/--decrypt",
				"note that password protected files are detected automatically")
		}
		if len(recipientFlags) > 0 {
			errorWithHint("-r/--recipient can't be used with -d/--decrypt",
				"did you mean to use -i/--identity to specify a private key?")
		}
		if len(recipientsFileFlags) > 0 {
			errorWithHint("-R/--recipients-file can't be used with -d/--decrypt",
				"did you mean to use -i/--identity to specify a private key?")
		}
	default: // encrypt
		if len(identityFlags) > 0 && !encryptFlag {
			errorWithHint("-i/--identity and -j can't be used in encryption mode unless symmetric encryption is explicitly selected with -e/--encrypt",
				"did you forget to specify -d/--decrypt?")
		}
		if len(recipientFlags)+len(recipientsFileFlags)+len(identityFlags) == 0 && !passFlag {
			errorWithHint("missing recipients",
				"did you forget to specify -r/--recipient, -R/--recipients-file or -p/--passphrase?")
		}
		if len(recipientFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -r/--recipient")
		}
		if len(recipientsFileFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -R/--recipients-file")
		}
		if len(identityFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -i/--identity and -j")
		}
	}

	var in io.Reader = os.Stdin
	var out io.Writer = os.Stdout
	if name := flag.Arg(0); name != "" && name != "-" {
		f, err := os.Open(name)
		if err != nil {
			errorf("failed to open input file %q: %v", name, err)
		}
		defer f.Close()
		in = f
	} else {
		stdinInUse = true
		if decryptFlag && term.IsTerminal(int(os.Stdin.Fd())) {
			// If the input comes from a TTY, assume it's armored, and buffer up
			// to the END line (or EOF/EOT) so that a password prompt or the
			// output don't get in the way of typing the input. See Issue 364.
			buf, err := bufferTerminalInput(in)
			if err != nil {
				errorf("failed to buffer terminal input: %v", err)
			}
			in = buf
		}
	}
	if name := outFlag; name != "" && name != "-" {
		f := newLazyOpener(name)
		defer func() {
			if err := f.Close(); err != nil {
				errorf("failed to close output file %q: %v", name, err)
			}
		}()
		out = f
	} else if term.IsTerminal(int(os.Stdout.Fd())) {
		if name != "-" {
			if decryptFlag {
				// TODO: buffer the output and check it's printable.
			} else if !armorFlag {
				// If the output wouldn't be armored, refuse to send binary to
				// the terminal unless explicitly requested with "-o -".
				errorWithHint("refusing to output binary to the terminal",
					"did you mean to use -a/--armor?",
					`force anyway with "-o -"`)
			}
		}
		if in == os.Stdin && term.IsTerminal(int(os.Stdin.Fd())) {
			// If the input comes from a TTY and output will go to a TTY,
			// buffer it up so it doesn't get in the way of typing the input.
			buf := &bytes.Buffer{}
			defer func() { io.Copy(os.Stdout, buf) }()
			out = buf
		}
	}

	switch {
	case decryptFlag && len(identityFlags) == 0:
		decryptPass(in, out)
	case decryptFlag:
		decryptNotPass(identityFlags, in, out)
	case passFlag:
		encryptPass(in, out, armorFlag)
	default:
		encryptNotPass(recipientFlags, recipientsFileFlags, identityFlags, in, out, armorFlag)
	}
}

func passphrasePromptForEncryption() (string, error) {
	pass, err := readSecret("Enter passphrase (leave empty to autogenerate a secure one):")
	if err != nil {
		return "", fmt.Errorf("could not read passphrase: %v", err)
	}
	p := string(pass)
	if p == "" {
		var words []string
		for i := 0; i < 10; i++ {
			words = append(words, randomWord())
		}
		p = strings.Join(words, "-")
		err := printfToTerminal("using autogenerated passphrase %q", p)
		if err != nil {
			return "", fmt.Errorf("could not print passphrase: %v", err)
		}
	} else {
		confirm, err := readSecret("Confirm passphrase:")
		if err != nil {
			return "", fmt.Errorf("could not read passphrase: %v", err)
		}
		if string(confirm) != p {
			return "", fmt.Errorf("passphrases didn't match")
		}
	}
	return p, nil
}

func encryptNotPass(recs, files []string, identities identityFlags, in io.Reader, out io.Writer, armor bool) {
	var recipients []age.Recipient
	for _, arg := range recs {
		r, err := parseRecipient(arg)
		if err, ok := err.(gitHubRecipientError); ok {
			errorWithHint(err.Error(), "instead, use recipient files like",
				"    curl -O https://github.com/"+err.username+".keys",
				"    age -R "+err.username+".keys")
		}
		if err != nil {
			errorf("%v", err)
		}
		recipients = append(recipients, r)
	}
	for _, name := range files {
		recs, err := parseRecipientsFile(name)
		if err != nil {
			errorf("failed to parse recipient file %q: %v", name, err)
		}
		recipients = append(recipients, recs...)
	}
	for _, f := range identities {
		switch f.Type {
		case "i":
			ids, err := parseIdentitiesFile(f.Value)
			if err != nil {
				errorf("reading %q: %v", f.Value, err)
			}
			r, err := identitiesToRecipients(ids)
			if err != nil {
				errorf("internal error processing %q: %v", f.Value, err)
			}
			recipients = append(recipients, r...)
		case "j":
			id, err := plugin.NewIdentityWithoutData(f.Value, pluginTerminalUI)
			if err != nil {
				errorf("initializing %q: %v", f.Value, err)
			}
			recipients = append(recipients, id.Recipient())
		}
	}
	encrypt(recipients, in, out, armor)
}

func encryptPass(in io.Reader, out io.Writer, armor bool) {
	pass, err := passphrasePromptForEncryption()
	if err != nil {
		errorf("%v", err)
	}

	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		errorf("%v", err)
	}
	testOnlyConfigureScryptIdentity(r)
	encrypt([]age.Recipient{r}, in, out, armor)
}

var testOnlyConfigureScryptIdentity = func(*age.ScryptRecipient) {}

func encrypt(recipients []age.Recipient, in io.Reader, out io.Writer, withArmor bool) {
	if withArmor {
		a := armor.NewWriter(out)
		defer func() {
			if err := a.Close(); err != nil {
				errorf("%v", err)
			}
		}()
		out = a
	}
	w, err := age.Encrypt(out, recipients...)
	if err != nil {
		errorf("%v", err)
	}
	if _, err := io.Copy(w, in); err != nil {
		errorf("%v", err)
	}
	if err := w.Close(); err != nil {
		errorf("%v", err)
	}
}

// crlfMangledIntro and utf16MangledIntro are the intro lines of the age format
// after mangling by various versions of PowerShell redirection, truncated to
// the length of the correct intro line. See issue 290.
const crlfMangledIntro = "age-encryption.org/v1" + "\r"
const utf16MangledIntro = "\xff\xfe" + "a\x00g\x00e\x00-\x00e\x00n\x00c\x00r\x00y\x00p\x00"

type rejectScryptIdentity struct{}

func (rejectScryptIdentity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	if len(stanzas) != 1 || stanzas[0].Type != "scrypt" {
		return nil, age.ErrIncorrectIdentity
	}
	errorWithHint("file is passphrase-encrypted but identities were specified with -i/--identity or -j",
		"remove all -i/--identity/-j flags to decrypt passphrase-encrypted files")
	panic("unreachable")
}

func decryptNotPass(flags identityFlags, in io.Reader, out io.Writer) {
	identities := []age.Identity{rejectScryptIdentity{}}

	for _, f := range flags {
		switch f.Type {
		case "i":
			ids, err := parseIdentitiesFile(f.Value)
			if err != nil {
				errorf("reading %q: %v", f.Value, err)
			}
			identities = append(identities, ids...)
		case "j":
			id, err := plugin.NewIdentityWithoutData(f.Value, pluginTerminalUI)
			if err != nil {
				errorf("initializing %q: %v", f.Value, err)
			}
			identities = append(identities, id)
		}
	}

	decrypt(identities, in, out)
}

func decryptPass(in io.Reader, out io.Writer) {
	identities := []age.Identity{
		// If there is an scrypt recipient (it will have to be the only one and)
		// this identity will be invoked.
		&LazyScryptIdentity{passphrasePromptForDecryption},
	}

	decrypt(identities, in, out)
}

func decrypt(identities []age.Identity, in io.Reader, out io.Writer) {
	rr := bufio.NewReader(in)
	if intro, _ := rr.Peek(len(crlfMangledIntro)); string(intro) == crlfMangledIntro ||
		string(intro) == utf16MangledIntro {
		errorWithHint("invalid header intro",
			"it looks like this file was corrupted by PowerShell redirection",
			"consider using -o or -a to encrypt files in PowerShell")
	}

	if start, _ := rr.Peek(len(armor.Header)); string(start) == armor.Header {
		in = armor.NewReader(rr)
	} else {
		in = rr
	}

	r, err := age.Decrypt(in, identities...)
	if err != nil {
		errorf("%v", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		errorf("%v", err)
	}
}

func passphrasePromptForDecryption() (string, error) {
	pass, err := readSecret("Enter passphrase:")
	if err != nil {
		return "", fmt.Errorf("could not read passphrase: %v", err)
	}
	return string(pass), nil
}

func identitiesToRecipients(ids []age.Identity) ([]age.Recipient, error) {
	var recipients []age.Recipient
	for _, id := range ids {
		switch id := id.(type) {
		case *age.X25519Identity:
			recipients = append(recipients, id.Recipient())
		case *plugin.Identity:
			recipients = append(recipients, id.Recipient())
		case *agessh.RSAIdentity:
			recipients = append(recipients, id.Recipient())
		case *agessh.Ed25519Identity:
			recipients = append(recipients, id.Recipient())
		case *agessh.EncryptedSSHIdentity:
			recipients = append(recipients, id.Recipient())
		case *EncryptedIdentity:
			r, err := id.Recipients()
			if err != nil {
				return nil, err
			}
			recipients = append(recipients, r...)
		default:
			return nil, fmt.Errorf("unexpected identity type: %T", id)
		}
	}
	return recipients, nil
}

type lazyOpener struct {
	name string
	f    *os.File
	err  error
}

func newLazyOpener(name string) io.WriteCloser {
	return &lazyOpener{name: name}
}

func (l *lazyOpener) Write(p []byte) (n int, err error) {
	if l.f == nil && l.err == nil {
		l.f, l.err = os.Create(l.name)
	}
	if l.err != nil {
		return 0, l.err
	}
	return l.f.Write(p)
}

func (l *lazyOpener) Close() error {
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}
