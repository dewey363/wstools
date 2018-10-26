package cli

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

const memMaxSize = 10 << 20 // 10MB

// MailRun 发送邮件
func MailRun(mailConfig *MailConfig) error {
	if mailConfig.User == "" || mailConfig.Passwd == "" || mailConfig.From == "" || mailConfig.To == "" {
		return fmt.Errorf("参数错误")
	}

	size, err := mailConfig.Len()
	if err != nil {
		fmt.Printf("获取邮件大小失败:%s\n", err.Error())
		return nil
	}

	var file io.ReadWriter
	if size >= memMaxSize {
		temp := fmt.Sprintf(".%d.tmp", os.Getpid())
		file, err = os.Create(temp)
		if err != nil {
			return fmt.Errorf("创建临时文件失败:%s", err.Error())
		}
		defer os.Remove(temp)
	} else {
		file = bytes.NewBuffer(make([]byte, 0, size))
	}
	err = mailConfig.Writer(file)
	if err != nil {
		return fmt.Errorf("封装邮件内容失败:%s", err)
	}

	auth := smtp.PlainAuth("", mailConfig.User, mailConfig.Passwd, strings.Split(mailConfig.Host, ":")[0])
	err = MailSend(mailConfig, auth, file)
	if c, ok := file.(io.Closer); ok {
		c.Close()
	}
	return err
}

// MailSend 送邮件
func MailSend(msg *MailConfig, auth smtp.Auth, body io.Reader) error {
	to := strings.Split(msg.To, ",")
	if msg.From == "" || len(to) == 0 {
		return errors.New("Must specify at least one From address and one To address")
	}
	client, err := smtp.Dial(msg.Host)
	if err != nil {
		return err
	}
	defer client.Close()

	host := strings.Split(msg.Host, ":")[0]
	if err = client.Hello(host); err != nil {
		return err
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		config := &tls.Config{ServerName: host}
		if err = client.StartTLS(config); err != nil {
			return err
		}
	}

	if err = client.Auth(auth); err != nil {
		return err
	}

	if err = client.Mail(msg.From); err != nil {
		return err
	}

	for _, addr := range to {
		if err = client.Rcpt(addr); err != nil {
			return err
		}
	}

	w, err := client.Data()
	if err != nil {
		return err
	}

	if value, ok := body.(io.Seeker); ok {
		value.Seek(0, 0)
	}

	_, err = io.Copy(w, body)
	if err != nil {
		return err
	}

	err = w.Close()
	if err != nil {
		return err
	}

	return client.Quit()
}

// MailConfig mail config args
type MailConfig struct {
	User, Passwd, Host string
	From, To, Type     string
	Subject, Content   string
	ContentPath        string
	Attachments        string
}

// Headers 返回邮件头信息
func (e *MailConfig) Headers() (textproto.MIMEHeader, error) {
	res := make(textproto.MIMEHeader)
	if _, ok := res["To"]; !ok && len(e.To) > 0 {
		res.Set("To", e.To)
	}

	if _, ok := res["Subject"]; !ok && e.Subject != "" {
		res.Set("Subject", e.Subject)
	}

	if _, ok := res["From"]; !ok {
		res.Set("From", e.From)
	}
	return res, nil
}

// Writer 写入附件
func (e *MailConfig) Writer(datawriter io.Writer) error {
	headers, err := e.Headers()
	if err != nil {
		return err
	}
	w := multipart.NewWriter(datawriter)

	headers.Set("Content-Type", "multipart/mixed;\r\n boundary="+w.Boundary())
	headerToBytes(datawriter, headers)
	io.WriteString(datawriter, "\r\n")

	fmt.Fprintf(datawriter, "--%s\r\n", w.Boundary())
	header := textproto.MIMEHeader{}
	if e.Content != "" || e.ContentPath != "" {
		subWriter := multipart.NewWriter(datawriter)
		header.Set("Content-Type", fmt.Sprintf("multipart/alternative;\r\n boundary=%s\r\n", subWriter.Boundary()))
		headerToBytes(datawriter, header)
		if e.Content != "" {
			header.Set("Content-Type", fmt.Sprintf("text/%s; charset=UTF-8", e.Type))
			header.Set("Content-Transfer-Encoding", "quoted-printable")
			if _, err := subWriter.CreatePart(header); err != nil {
				return err
			}
			qp := quotedprintable.NewWriter(datawriter)
			if _, err := qp.Write([]byte(e.Content)); err != nil {
				return err
			}
			if err := qp.Close(); err != nil {
				return err
			}
		} else {
			header.Set("Content-Type", fmt.Sprintf("text/%s; charset=UTF-8", e.Type))
			header.Set("Content-Transfer-Encoding", "quoted-printable")
			if _, err := subWriter.CreatePart(header); err != nil {
				return err
			}
			qp := quotedprintable.NewWriter(datawriter)
			File, err := os.Open(e.ContentPath)
			if err != nil {
				return err
			}
			defer File.Close()

			_, err = io.Copy(qp, File)
			if err != nil {
				return err
			}
			if err := qp.Close(); err != nil {
				return err
			}
		}
		if err := subWriter.Close(); err != nil {
			return err
		}
	}
	if e.Attachments != "" {
		list := strings.Split(e.Attachments, ",")
		for _, path := range list {
			err = attach(w, path)
			if err != nil {
				w.Close()
				return err
			}
		}
	}
	return nil
}

// Len 获取邮件大小
func (e *MailConfig) Len() (int64, error) {
	var l int64
	if e.Content != "" {
		l += int64(len(e.Content))
	} else if e.ContentPath != "" {
		stat, err := os.Lstat(e.ContentPath)
		if err != nil {
			return 0, err
		}
		l += stat.Size()
	} else {
		return 0, nil
	}
	if e.Attachments != "" {
		for _, path := range strings.Split(e.Attachments, ",") {
			stat, err := os.Lstat(path)
			if err != nil {
				return 0, err
			}
			l += stat.Size()
		}
	}
	return l, nil

}
func headerToBytes(w io.Writer, header textproto.MIMEHeader) {
	for field, vals := range header {
		for _, subval := range vals {
			io.WriteString(w, field)
			io.WriteString(w, ": ")
			switch {
			case field == "Content-Type" || field == "Content-Disposition":
				w.Write([]byte(subval))
			default:
				w.Write([]byte(mime.QEncoding.Encode("UTF-8", subval)))
			}
			io.WriteString(w, "\r\n")
		}
	}
}

// attach 封装附件
func attach(w *multipart.Writer, filename string) (err error) {
	typ := mime.TypeByExtension(filepath.Ext(filename))
	var Header = make(textproto.MIMEHeader)
	if typ != "" {
		Header.Set("Content-Type", typ)
	} else {
		Header.Set("Content-Type", "application/octet-stream")
	}
	basename := filepath.Base(filename)
	Header.Set("Content-Disposition", fmt.Sprintf("attachment;\r\n filename=\"%s\"", basename))
	Header.Set("Content-ID", fmt.Sprintf("<%s>", basename))
	Header.Set("Content-Transfer-Encoding", "base64")
	File, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer File.Close()

	mw, err := w.CreatePart(Header)
	if err != nil {
		return err
	}
	return base64Wrap(mw, File)
}

func base64Wrap(w io.Writer, r io.Reader) error {
	const maxRaw = 57
	const MaxLineLength = 76

	buffer := make([]byte, MaxLineLength+len("\r\n"))
	copy(buffer[MaxLineLength:], "\r\n")
	var b = make([]byte, maxRaw)
	for {
		n, err := r.Read(b)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if n == maxRaw {
			base64.StdEncoding.Encode(buffer, b[:n])
			w.Write(buffer)
		} else {
			out := buffer[:base64.StdEncoding.EncodedLen(len(b))]
			base64.StdEncoding.Encode(out, b)
			out = append(out, "\r\n"...)
			w.Write(out)
		}
	}
}
