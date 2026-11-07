package ims

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"vocat/internal/vowifi"
)

func TestUSSISDPRtpmapsCarryClockRate(t *testing.T) {
	sdp := string(buildUSSISDP(net.ParseIP("192.0.2.1")))
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a=rtpmap:") && !strings.Contains(line, "/") {
			t.Fatalf("rtpmap without clock rate: %q", line)
		}
	}
	for _, want := range []string{"a=rtpmap:97 AMR/8000/1\r\n", "a=rtpmap:96 telephone-event/8000\r\n"} {
		if !strings.Contains(sdp, want) {
			t.Fatalf("USSI SDP missing %q:\n%s", want, sdp)
		}
	}
}

func TestUSSIRejectedErrorKeepsReasonAndWarning(t *testing.T) {
	err := ussiRejectedError(&sipResponse{StatusCode: 606, Reason: "Not Acceptable", Headers: map[string][]string{
		"warning": {`399 anonymous.invalid "Transient failure or protocol error (Diameter)"`},
	}})
	want := `ims: USSI rejected with SIP 606 (Not Acceptable; Warning: 399 anonymous.invalid "Transient failure or protocol error (Diameter)")`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if got := ussiRejectedError(&sipResponse{StatusCode: 488}).Error(); got != "ims: USSI rejected with SIP 488" {
		t.Fatalf("bare error = %q", got)
	}
}

func TestUSSDXMLSerializationAndParsing(t *testing.T) {
	// 1. Build and parse basic XML
	xmlData := buildUSSDXML("*123#", "en")
	if !strings.Contains(string(xmlData), "<ussd-string>*123#</ussd-string>") {
		t.Fatalf("buildUSSDXML missing string: %s", string(xmlData))
	}
	if !strings.Contains(string(xmlData), "<language>en</language>") {
		t.Fatalf("buildUSSDXML missing language: %s", string(xmlData))
	}

	text, lang, errCode, err := parseUSSDXML(xmlData)
	if err != nil {
		t.Fatalf("parseUSSDXML error: %v", err)
	}
	if text != "*123#" || lang != "en" || errCode != nil {
		t.Fatalf("parseUSSDXML = (%q, %q, %v), want (*123#, en, nil)", text, lang, errCode)
	}

	// 2. Parse XML with error code
	errXML := []byte(`<?xml version="1.0" encoding="UTF-8"?><ussd-data xmlns="urn:oma:xml:ussd:ussd-data"><error-code>7</error-code></ussd-data>`)
	text, lang, errCode, err = parseUSSDXML(errXML)
	if err != nil {
		t.Fatalf("parseUSSDXML error-code error: %v", err)
	}
	if errCode == nil || *errCode != 7 {
		t.Fatalf("parseUSSDXML errCode = %v, want 7", errCode)
	}

	// 3. Multipart extraction
	sdp := buildUSSISDP(net.ParseIP("192.0.2.1"))
	boundary := "boundary-test-123"
	multipart := buildUSSIMultipartBody(sdp, xmlData, boundary)
	extractedText, extractedLang, _, extractedErrCode, err := extractUSSDPayloadFromSIP(multipart, "multipart/mixed; boundary="+boundary)
	if err != nil {
		t.Fatalf("extractUSSDPayloadFromSIP error: %v", err)
	}
	if extractedText != "*123#" || extractedLang != "en" || extractedErrCode != nil {
		t.Fatalf("extracted multipart payload = (%q, %q, %v), want (*123#, en, nil)",
			extractedText, extractedLang, extractedErrCode)
	}
}

func TestSessionUSSI_MenuDialogAndReply(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(10 * time.Second))

	serverDone := make(chan error, 1)
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 32))

	go func() {
		packet := make([]byte, 65535)

		// Registration exchange
		count, remote, err := listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err := parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		regCallID := headers["call-id"]
		if _, err = listener.WriteToUDP(testResponse(401, "Unauthorized", regCallID, headers["cseq"], []string{
			`WWW-Authenticate: Digest realm="ims.mnc001.mcc001.3gppnetwork.org", nonce="` + nonce + `", algorithm=AKAv1-MD5, qop="auth"`,
		}), remote); err != nil {
			serverDone <- err
			return
		}

		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		if _, err = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], []string{
			"Contact: " + headers["contact"] + ";expires=600",
		}), remote); err != nil {
			serverDone <- err
			return
		}

		// 1. Initial INVITE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		invMsg, err := parseSIPPacket(packet[:count])
		if err != nil || invMsg.Request == nil || invMsg.Request.Method != "INVITE" {
			serverDone <- fmt.Errorf("expected INVITE, got %#v", invMsg)
			return
		}

		invCallID := invMsg.Request.value("Call-ID")
		invCSeq := invMsg.Request.value("CSeq")
		from := invMsg.Request.value("From")
		to := invMsg.Request.value("To") + ";tag=srv-tag-menu"
		clientContact := headerURI(invMsg.Request.value("Contact"))

		sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 0 RTP/AVP 96\r\n"
		inv200 := []byte(strings.Join([]string{
			"SIP/2.0 200 OK",
			"Via: " + invMsg.Request.value("Via"),
			"From: " + from,
			"To: " + to,
			"Call-ID: " + invCallID,
			"CSeq: " + invCSeq,
			"Contact: <sip:as@127.0.0.1:5060>",
			"Content-Type: application/sdp",
			fmt.Sprintf("Content-Length: %d", len(sdp)),
			"", "",
		}, "\r\n") + sdp)
		if _, err = listener.WriteToUDP(inv200, remote); err != nil {
			serverDone <- err
			return
		}

		// 2. UE ACK
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		ackMsg, err := parseSIPPacket(packet[:count])
		if err != nil || ackMsg.Request == nil || ackMsg.Request.Method != "ACK" {
			serverDone <- fmt.Errorf("expected ACK, got %#v", ackMsg)
			return
		}

		// 3. Network sends in-dialog INFO (Menu)
		menuXML := `<?xml version="1.0" encoding="UTF-8"?><ussd-data xmlns="urn:oma:xml:ussd:ussd-data"><language>en</language><ussd-string>1. Balance&#10;2. Plans</ussd-string></ussd-data>`
		infoReq := []byte(strings.Join([]string{
			"INFO " + clientContact + " SIP/2.0",
			"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKinfomenu1",
			"Max-Forwards: 70",
			"From: " + to,
			"To: " + from,
			"Call-ID: " + invCallID,
			"CSeq: 10 INFO",
			"Info-Package: g.3gpp.ussd",
			"Content-Type: application/vnd.3gpp.ussd+xml",
			"Content-Disposition: info-package",
			fmt.Sprintf("Content-Length: %d", len(menuXML)),
			"", "",
		}, "\r\n") + menuXML)
		if _, err = listener.WriteToUDP(infoReq, remote); err != nil {
			serverDone <- err
			return
		}

		// 4. UE replies 200 OK to INFO
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		infoResp, err := parseSIPPacket(packet[:count])
		if err != nil || infoResp.Response == nil || infoResp.Response.StatusCode != 200 {
			serverDone <- fmt.Errorf("expected 200 OK for INFO, got %#v", infoResp)
			return
		}

		// 5. UE sends in-dialog INFO with choice "1"
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		ueInfo, err := parseSIPPacket(packet[:count])
		if err != nil || ueInfo.Request == nil || ueInfo.Request.Method != "INFO" {
			serverDone <- fmt.Errorf("expected UE INFO, got %#v", ueInfo)
			return
		}
		choiceText, _, _, _, _ := extractUSSDPayloadFromSIP(ueInfo.Request.Body, ueInfo.Request.value("Content-Type"))
		if choiceText != "1" {
			serverDone <- fmt.Errorf("expected choice '1', got %q", choiceText)
			return
		}

		// 6. Network replies 200 OK to UE's INFO
		choice200 := []byte(strings.Join([]string{
			"SIP/2.0 200 OK",
			"Via: " + ueInfo.Request.value("Via"),
			"From: " + ueInfo.Request.value("From"),
			"To: " + ueInfo.Request.value("To"),
			"Call-ID: " + ueInfo.Request.value("Call-ID"),
			"CSeq: " + ueInfo.Request.value("CSeq"),
			"Content-Length: 0",
			"", "",
		}, "\r\n"))
		if _, err = listener.WriteToUDP(choice200, remote); err != nil {
			serverDone <- err
			return
		}

		// 7. Network sends BYE with final balance
		finalXML := `<?xml version="1.0" encoding="UTF-8"?><ussd-data xmlns="urn:oma:xml:ussd:ussd-data"><language>en</language><ussd-string>Your balance is $25.00</ussd-string></ussd-data>`
		byeReq := []byte(strings.Join([]string{
			"BYE " + clientContact + " SIP/2.0",
			"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKbyefinal1",
			"Max-Forwards: 70",
			"From: " + to,
			"To: " + from,
			"Call-ID: " + invCallID,
			"CSeq: 11 BYE",
			"Content-Type: application/vnd.3gpp.ussd+xml",
			fmt.Sprintf("Content-Length: %d", len(finalXML)),
			"", "",
		}, "\r\n") + finalXML)
		if _, err = listener.WriteToUDP(byeReq, remote); err != nil {
			serverDone <- err
			return
		}

		// 8. UE replies 200 OK to BYE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		byeResp, err := parseSIPPacket(packet[:count])
		if err != nil || byeResp.Response == nil || byeResp.Response.StatusCode != 200 {
			serverDone <- fmt.Errorf("expected 200 OK for BYE, got %#v", byeResp)
			return
		}

		// De-registration
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err == nil && headers["expires"] == "0" {
			_, _ = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], nil), remote)
		}
		serverDone <- nil
	}()

	provider, err := NewProvider(
		smsTestAKA{&recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}}},
		Config{
			PCSCF: listener.LocalAddr().String(), LocalAddress: "127.0.0.1",
			Transport: "udp", TransactionTimeout: 3 * time.Second, SecurityMode: SecurityDisabled,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Start(context.Background(), vowifi.IMSRequest{
		DeviceID: "ec20",
		Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"},
		Tunnel: evidenceTunnel{evidence: vowifi.TunnelEvidence{
			Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{listener.LocalAddr().String()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sender := session.(vowifi.USSISender)

	// Step 1: Submit initial USSI code
	step1, err := sender.SendUSSI(context.Background(), vowifi.USSISubmitRequest{Code: "#999#"})
	if err != nil {
		t.Fatalf("Step 1 SendUSSI error: %v", err)
	}
	if step1.Status != "awaiting_input" || !step1.Continueable || step1.SessionID == "" {
		t.Fatalf("Step 1 result = %#v, want awaiting_input with non-empty SessionID", step1)
	}
	if !strings.Contains(step1.Text, "Balance") {
		t.Fatalf("Step 1 text = %q, want menu containing Balance", step1.Text)
	}

	// Step 2: Continue dialog with choice "1"
	step2, err := sender.SendUSSI(context.Background(), vowifi.USSISubmitRequest{
		SessionID: step1.SessionID,
		Input:     "1",
	})
	if err != nil {
		t.Fatalf("Step 2 SendUSSI error: %v", err)
	}
	if step2.Status != "final" || step2.Continueable || step2.Text != "Your balance is $25.00" {
		t.Fatalf("Step 2 result = %#v, want final with balance", step2)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestSessionUSSI_Cancel(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(10 * time.Second))

	serverDone := make(chan error, 1)
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 32))

	go func() {
		packet := make([]byte, 65535)

		// Registration
		count, remote, err := listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err := parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		regCallID := headers["call-id"]
		if _, err = listener.WriteToUDP(testResponse(401, "Unauthorized", regCallID, headers["cseq"], []string{
			`WWW-Authenticate: Digest realm="ims.mnc001.mcc001.3gppnetwork.org", nonce="` + nonce + `", algorithm=AKAv1-MD5, qop="auth"`,
		}), remote); err != nil {
			serverDone <- err
			return
		}

		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		if _, err = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], []string{
			"Contact: " + headers["contact"] + ";expires=600",
		}), remote); err != nil {
			serverDone <- err
			return
		}

		// 1. Initial INVITE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		invMsg, err := parseSIPPacket(packet[:count])
		if err != nil || invMsg.Request == nil || invMsg.Request.Method != "INVITE" {
			serverDone <- fmt.Errorf("expected INVITE, got %#v", invMsg)
			return
		}

		invCallID := invMsg.Request.value("Call-ID")
		invCSeq := invMsg.Request.value("CSeq")
		from := invMsg.Request.value("From")
		to := invMsg.Request.value("To") + ";tag=srv-tag-cancel"
		clientContact := headerURI(invMsg.Request.value("Contact"))

		sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 0 RTP/AVP 96\r\n"
		inv200 := []byte(strings.Join([]string{
			"SIP/2.0 200 OK",
			"Via: " + invMsg.Request.value("Via"),
			"From: " + from,
			"To: " + to,
			"Call-ID: " + invCallID,
			"CSeq: " + invCSeq,
			"Contact: <sip:as@127.0.0.1:5060>",
			"Content-Type: application/sdp",
			fmt.Sprintf("Content-Length: %d", len(sdp)),
			"", "",
		}, "\r\n") + sdp)
		if _, err = listener.WriteToUDP(inv200, remote); err != nil {
			serverDone <- err
			return
		}

		// 2. UE ACK
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		ackMsg, err := parseSIPPacket(packet[:count])
		if err != nil || ackMsg.Request == nil || ackMsg.Request.Method != "ACK" {
			serverDone <- fmt.Errorf("expected ACK, got %#v", ackMsg)
			return
		}

		// 3. Network sends INFO (prompt)
		promptXML := `<?xml version="1.0" encoding="UTF-8"?><ussd-data xmlns="urn:oma:xml:ussd:ussd-data"><language>en</language><ussd-string>Enter PIN:</ussd-string></ussd-data>`
		infoReq := []byte(strings.Join([]string{
			"INFO " + clientContact + " SIP/2.0",
			"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKinfoprompt1",
			"Max-Forwards: 70",
			"From: " + to,
			"To: " + from,
			"Call-ID: " + invCallID,
			"CSeq: 10 INFO",
			"Info-Package: g.3gpp.ussd",
			"Content-Type: application/vnd.3gpp.ussd+xml",
			"Content-Disposition: info-package",
			fmt.Sprintf("Content-Length: %d", len(promptXML)),
			"", "",
		}, "\r\n") + promptXML)
		if _, err = listener.WriteToUDP(infoReq, remote); err != nil {
			serverDone <- err
			return
		}

		// 4. UE replies 200 OK to INFO
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		infoResp, err := parseSIPPacket(packet[:count])
		if err != nil || infoResp.Response == nil || infoResp.Response.StatusCode != 200 {
			serverDone <- fmt.Errorf("expected 200 OK for INFO, got %#v", infoResp)
			return
		}

		// 5. UE calls CancelUSSI -> sends BYE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		byeMsg, err := parseSIPPacket(packet[:count])
		if err != nil || byeMsg.Request == nil || byeMsg.Request.Method != "BYE" {
			serverDone <- fmt.Errorf("expected BYE on cancel, got %#v", byeMsg)
			return
		}

		// 6. Network replies 200 OK to BYE
		bye200 := []byte(strings.Join([]string{
			"SIP/2.0 200 OK",
			"Via: " + byeMsg.Request.value("Via"),
			"From: " + byeMsg.Request.value("From"),
			"To: " + byeMsg.Request.value("To"),
			"Call-ID: " + byeMsg.Request.value("Call-ID"),
			"CSeq: " + byeMsg.Request.value("CSeq"),
			"Content-Length: 0",
			"", "",
		}, "\r\n"))
		if _, err = listener.WriteToUDP(bye200, remote); err != nil {
			serverDone <- err
			return
		}

		// De-registration
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err == nil && headers["expires"] == "0" {
			_, _ = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], nil), remote)
		}
		serverDone <- nil
	}()

	provider, err := NewProvider(
		smsTestAKA{&recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}}},
		Config{
			PCSCF: listener.LocalAddr().String(), LocalAddress: "127.0.0.1",
			Transport: "udp", TransactionTimeout: 3 * time.Second, SecurityMode: SecurityDisabled,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Start(context.Background(), vowifi.IMSRequest{
		DeviceID: "ec20",
		Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"},
		Tunnel: evidenceTunnel{evidence: vowifi.TunnelEvidence{
			Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{listener.LocalAddr().String()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sender := session.(vowifi.USSISender)
	step1, err := sender.SendUSSI(context.Background(), vowifi.USSISubmitRequest{Code: "*555#"})
	if err != nil {
		t.Fatalf("SendUSSI error: %v", err)
	}
	if step1.Status != "awaiting_input" {
		t.Fatalf("expected awaiting_input, got %s", step1.Status)
	}

	canceler := session.(vowifi.USSICanceler)
	if err := canceler.CancelUSSI(context.Background(), step1.SessionID); err != nil {
		t.Fatalf("CancelUSSI error: %v", err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestSessionUSSI_LegacyFallbackOn415(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(10 * time.Second))

	serverDone := make(chan error, 1)
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 32))

	go func() {
		packet := make([]byte, 65535)

		// Registration
		count, remote, err := listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err := parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		regCallID := headers["call-id"]
		if _, err = listener.WriteToUDP(testResponse(401, "Unauthorized", regCallID, headers["cseq"], []string{
			`WWW-Authenticate: Digest realm="ims.mnc001.mcc001.3gppnetwork.org", nonce="` + nonce + `", algorithm=AKAv1-MD5, qop="auth"`,
		}), remote); err != nil {
			serverDone <- err
			return
		}

		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err != nil {
			serverDone <- err
			return
		}
		if _, err = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], []string{
			"Contact: " + headers["contact"] + ";expires=600",
		}), remote); err != nil {
			serverDone <- err
			return
		}

		// 1. Initial INVITE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		invMsg, err := parseSIPPacket(packet[:count])
		if err != nil || invMsg.Request == nil || invMsg.Request.Method != "INVITE" {
			serverDone <- fmt.Errorf("expected INVITE, got %#v", invMsg)
			return
		}

		// 2. Network rejects INVITE with 415 Unsupported Media Type
		inv415 := []byte(strings.Join([]string{
			"SIP/2.0 415 Unsupported Media Type",
			"Via: " + invMsg.Request.value("Via"),
			"From: " + invMsg.Request.value("From"),
			"To: " + invMsg.Request.value("To") + ";tag=srv-reject",
			"Call-ID: " + invMsg.Request.value("Call-ID"),
			"CSeq: " + invMsg.Request.value("CSeq"),
			"Accept: application/vnd.3gpp.ussd",
			"Content-Length: 0",
			"", "",
		}, "\r\n"))
		if _, err = listener.WriteToUDP(inv415, remote); err != nil {
			serverDone <- err
			return
		}

		// 3. UE sends ACK for 415
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		ackMsg, err := parseSIPPacket(packet[:count])
		if err != nil || ackMsg.Request == nil || ackMsg.Request.Method != "ACK" {
			serverDone <- fmt.Errorf("expected ACK for 415, got %#v", ackMsg)
			return
		}

		// 4. UE falls back to standalone SIP MESSAGE
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		msgMsg, err := parseSIPPacket(packet[:count])
		if err != nil || msgMsg.Request == nil || msgMsg.Request.Method != "MESSAGE" {
			serverDone <- fmt.Errorf("expected fallback MESSAGE, got %#v", msgMsg)
			return
		}
		if msgMsg.Request.value("Content-Type") != ussiContentType {
			serverDone <- fmt.Errorf("fallback MESSAGE content-type = %s, want %s",
				msgMsg.Request.value("Content-Type"), ussiContentType)
			return
		}

		// Network replies 200 OK with USSD binary body
		replyBody := buildUSSDBody("Legacy Fallback Reply")
		msg200 := []byte(strings.Join([]string{
			"SIP/2.0 200 OK",
			"Call-ID: " + msgMsg.Request.value("Call-ID"),
			"CSeq: " + msgMsg.Request.value("CSeq"),
			"Content-Type: application/vnd.3gpp.ussd",
			"Content-Transfer-Encoding: binary",
			fmt.Sprintf("Content-Length: %d", len(replyBody)),
			"", "",
		}, "\r\n"))
		msg200 = append(msg200, replyBody...)
		if _, err = listener.WriteToUDP(msg200, remote); err != nil {
			serverDone <- err
			return
		}

		// De-registration
		count, remote, err = listener.ReadFromUDP(packet)
		if err != nil {
			serverDone <- err
			return
		}
		_, headers, err = parseTestRequest(packet[:count])
		if err == nil && headers["expires"] == "0" {
			_, _ = listener.WriteToUDP(testResponse(200, "OK", regCallID, headers["cseq"], nil), remote)
		}
		serverDone <- nil
	}()

	provider, err := NewProvider(
		smsTestAKA{&recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}}},
		Config{
			PCSCF: listener.LocalAddr().String(), LocalAddress: "127.0.0.1",
			Transport: "udp", TransactionTimeout: 3 * time.Second, SecurityMode: SecurityDisabled,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := provider.Start(context.Background(), vowifi.IMSRequest{
		DeviceID: "ec20",
		Identity: vowifi.SIMIdentity{IMSI: "001010123456789", HomeMCC: "001", HomeMNC: "01"},
		Tunnel: evidenceTunnel{evidence: vowifi.TunnelEvidence{
			Established: true, LocalIPv4: "127.0.0.1", PCSCF: []string{listener.LocalAddr().String()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := session.(vowifi.USSISender).SendUSSI(context.Background(), vowifi.USSISubmitRequest{Code: "*99#"})
	if err != nil {
		t.Fatalf("fallback SendUSSI error: %v", err)
	}
	if result.Status != "final" || result.Text != "Legacy Fallback Reply" {
		t.Fatalf("fallback result = %#v, want Legacy Fallback Reply", result)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}
