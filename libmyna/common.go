package libmyna

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/hamano/pkcs7"
	"github.com/urfave/cli"
	"io/ioutil"
	"strings"
)

func ToBytes(s string) []byte {
	b, _ := hex.DecodeString(strings.Replace(s, " ", "", -1))
	return b
}

func ToHexString(b []byte) string {
	s := hex.EncodeToString(b)
	return s
}

func Ready(c *cli.Context) (*Reader, error) {
	reader := NewReader(c)
	if reader == nil {
		return nil, errors.New("リーダーが見つかりません。")
	}
	err := reader.WaitForCard()
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func CheckCard(c *cli.Context) error {
	reader, err := Ready(c)
	if err != nil {
		return err
	}
	defer reader.Finalize()
	var sw1, sw2 uint8
	if !reader.SelectAP("D3 92 f0 00 26 01 00 00 00 01") {
		return errors.New("これは個人番号カードではありません。")
	}

	sw1, sw2 = reader.SelectEF("00 06")
	if !(sw1 == 0x90 && sw2 == 0x00) {
		return errors.New("トークン情報を取得できません。")
	}

	var data []byte
	data = reader.ReadBinary(0x20)
	token := string(bytes.TrimRight(data, " "))
	if token == "JPKIAPICCTOKEN2" {
		return nil
	} else if token == "JPKIAPICCTOKEN" {
		return errors.New("これは住基カードですね?")
	} else {
		return fmt.Errorf("不明なトークン情報: %s", token)
	}
}

func GetCardInfo(c *cli.Context, pin string) (map[string]string, error) {
	reader := NewReader(c)
	if reader == nil {
		return nil, errors.New("リーダーが見つかりません。")
	}
	defer reader.Finalize()
	err := reader.WaitForCard()
	if err != nil {
		return nil, err
	}

	reader.SelectAP("D3 92 10 00 31 00 01 01 04 08")
	reader.SelectEF("00 11") // 券面入力補助PIN IEF
	sw1, sw2 := reader.Verify(pin)
	if !(sw1 == 0x90 && sw2 == 0x00) {
		return nil, errors.New("暗証番号が間違っています。")
	}
	reader.SelectEF("00 01")
	data := reader.ReadBinary(16)
	var number asn1.RawValue
	asn1.Unmarshal(data[1:], &number)

	reader.SelectEF("00 02")
//	data = reader.ReadBinary(5)
//	if len(data) != 5 {
	data = reader.ReadBinary(3)
	if len(data) != 3 {
		return nil, errors.New("Error at ReadBinary()")
	}
//	data_size := uint16(data[3])<<8 | uint16(data[4])
//	data = reader.ReadBinary(5 + data_size)
	data_size := uint16(data[2])
	data = reader.ReadBinary(3 + data_size)
	var attr [5]asn1.RawValue
//	pos := 5
	pos := 3
	for i := 0; i < 5; i++ {
		asn1.Unmarshal(data[pos:], &attr[i])
		pos += len(attr[i].FullBytes)
	}

	info := map[string]string{}
	info["number"] = string(number.Bytes)
	info["header"] = fmt.Sprintf("% X", attr[0].Bytes)
	info["name"] = string(attr[1].Bytes)
	info["address"] = string(attr[2].Bytes)
	info["birth"] = string(attr[3].Bytes)
	info["sex"] = string(attr[4].Bytes)
	return info, nil
}

func GetPinStatus(c *cli.Context) (map[string]int, error) {
	reader, err := Ready(c)
	if err != nil {
		return nil, err
	}
	defer reader.Finalize()

	status := map[string]int{}

	reader.SelectAP("D3 92 f0 00 26 01 00 00 00 01") // 公的個人認証
	reader.SelectEF("00 18")                         // IEF for AUTH
	status["auth"] = reader.LookupPin()

	reader.SelectEF("00 1B") // IEF for SIGN
	status["sign"] = reader.LookupPin()

	reader.SelectAP("D3 92 10 00 31 00 01 01 04 08") // 券面入力補助AP
	reader.SelectEF("00 11")                         // IEF
	status["card"] = reader.LookupPin()

	reader.SelectAP("D3 92 10 00 31 00 01 01 01 00") // 謎AP
	reader.SelectEF("00 1C")
	status["unknown1"] = reader.LookupPin()

	reader.SelectAP("D3 92 10 00 31 00 01 01 04 01") // 住基?
	reader.SelectEF("00 1C")
	status["unknown2"] = reader.LookupPin()
	return status, nil
}

func makeDigestInfo(hash []byte) []byte {
	var prefix = []byte{0x30, 0x21, 0x30, 0x09, // SEQUENCE { SEQUENCE {
		0x06, 0x05, 0x2b, 0x0e, 0x03, 0x02, 0x1a, // sha-1
		0x05, 0x00, // NULL }
		0x04, 0x14} // OCTET STRING
	/*
		var prefixSHA256 = []byte{0x30, 0x31, 0x30, 0x0d, // SEQUENCE { SEQUENCE {
			0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x01, // sha-256
			0x05, 0x00, // NULL }
			0x04, 0x20} // OCTET STRING
	*/
	return append(prefix, hash...)
}

type ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

func Sign(c *cli.Context, pin string, in string, out string) error {
	rawContent, err := ioutil.ReadFile(in)
	if err != nil {
		return err
	}

	toBeSigned, err := pkcs7.NewSignedData(rawContent)
	if err != nil {
		return err
	}

	// 署名用証明書の取得
	cert, err := GetCert(c, "00 01", pin)
	if err != nil {
		return err
	}
	attrs, hashed, err := toBeSigned.HashAttributes(crypto.SHA1, pkcs7.SignerInfoConfig{})
	if err != nil {
		return err
	}

	ias, err := pkcs7.Cert2issuerAndSerial(cert)
	if err != nil {
		return err
	}

	reader, err := Ready(c)
	if err != nil {
		return err
	}
	defer reader.Finalize()
	reader.SelectAP("D3 92 f0 00 26 01 00 00 00 01") // JPKI
	reader.SelectEF("00 1B")                         // IEF for SIGN
	sw1, sw2 := reader.Verify(pin)
	if !(sw1 == 0x90 && sw2 == 0x00) {
		return errors.New("暗証番号が間違っています。")
	}
	reader.SelectEF("00 1A") // Select SIGN EF
	digestInfo := makeDigestInfo(hashed)

	signature, err := reader.Signature(digestInfo)
	if err != nil {
		return err
	}

	oidDigestAlgorithmSHA1 := asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidEncryptionAlgorithmRSA := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	signerInfo := pkcs7.SignerInfo{
		AuthenticatedAttributes:   attrs,
		DigestAlgorithm:           pkix.AlgorithmIdentifier{Algorithm: oidDigestAlgorithmSHA1},
		DigestEncryptionAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidEncryptionAlgorithmRSA},
		IssuerAndSerialNumber:     ias,
		EncryptedDigest:           signature,
		Version:                   1,
	}
	toBeSigned.AddSignerInfo(cert, signerInfo)
	signed, err := toBeSigned.Finish()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(out, signed, 0664)
	if err != nil {
		return err
	}
	return nil
}

var oid2str = map[string]string{
	"2.5.4.3":  "CN",
	"2.5.4.6":  "C",
	"2.5.4.7":  "L",
	"2.5.4.10": "O",
	"2.5.4.11": "OU",
}

func Name2String(name pkix.Name) string {
	var dn []string
	for _, rdns := range name.ToRDNSequence() {
		for _, rdn := range rdns {
			value := rdn.Value.(string)
			if key, ok := oid2str[rdn.Type.String()]; ok {
				dn = append(dn, fmt.Sprintf("%s=%s", key, value))
			} else {
				dn = append(dn, fmt.Sprintf("%s=%s", rdn.Type.String(), value))
			}
		}
	}
	return strings.Join(dn, "/")
}

func GetCert(c *cli.Context, efid string, pin string) (*x509.Certificate, error) {
	reader, err := Ready(c)
	if err != nil {
		return nil, err
	}
	defer reader.Finalize()
	reader.SelectAP("D3 92 f0 00 26 01 00 00 00 01") // JPKI
	reader.SelectEF(efid)

	if pin != "" {
		reader.SelectEF("00 1B") // VERIFY EF for SIGN
		sw1, sw2 := reader.Verify(pin)
		if !(sw1 == 0x90 && sw2 == 0x00) {
			return nil, errors.New("暗証番号が間違っています。")
		}
	}

	reader.SelectEF(efid)
	data := reader.ReadBinary(4)
	if len(data) != 4 {
		return nil, errors.New("ReadBinary: invalid length")
	}
	data_size := uint16(data[2])<<8 | uint16(data[3])
	data = reader.ReadBinary(4 + data_size)
	cert, err := x509.ParseCertificate(data)
	if err != nil {
		return nil, err
	}
	return cert, nil
}
