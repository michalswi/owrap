package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"github.com/michalswi/pdf-reader/pdf"
)

// ExtractTextFromFile returns readable text for a file on disk, dispatching by
// extension to a format-specific extractor and falling back to a raw text read.
func ExtractTextFromFile(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return ReadPDFContent(path)
	case ".docx":
		return ReadDOCXContent(path)
	case ".pptx":
		return ReadPPTXContent(path)
	case ".doc":
		return ReadDOCContent(path)
	case ".pcap", ".pcapng", ".cap":
		return ReadPCAPContent(path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	if !looksLikeText(data) {
		return "", fmt.Errorf("unsupported binary file type %q", filepath.Ext(path))
	}
	return string(data), nil
}

func looksLikeText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	if !utf8.Valid(sample) {
		return false
	}
	textCount := 0
	for _, b := range sample {
		if b == '\n' || b == '\r' || b == '\t' || (b >= 32 && b < 127) {
			textCount++
		}
	}
	return float64(textCount)/float64(len(sample)) > 0.9
}

// ReadPDFContent extracts text from a PDF file.
func ReadPDFContent(path string) (string, error) {
	f, reader, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer f.Close()

	textReader, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("failed to extract text from PDF: %w", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(textReader); err != nil {
		return "", fmt.Errorf("failed to read extracted text: %w", err)
	}
	return buf.String(), nil
}

// ReadDOCXContent extracts text from a DOCX file.
func ReadDOCXContent(path string) (string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("failed to open DOCX: %w", err)
	}
	defer reader.Close()

	var parts []string
	for _, file := range reader.File {
		if isDOCXTextPart(strings.ToLower(file.Name)) {
			parts = append(parts, file.Name)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no readable document XML parts found in DOCX")
	}
	sort.Strings(parts)

	var content strings.Builder
	for _, partName := range parts {
		var partFile *zip.File
		for _, file := range reader.File {
			if file.Name == partName {
				partFile = file
				break
			}
		}
		if partFile == nil {
			continue
		}

		rc, err := partFile.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open DOCX part %s: %w", partName, err)
		}
		partText, parseErr := extractDOCXText(rc)
		closeErr := rc.Close()
		if parseErr != nil {
			return "", fmt.Errorf("failed to parse DOCX part %s: %w", partName, parseErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("failed to close DOCX part %s: %w", partName, closeErr)
		}

		partText = strings.TrimSpace(partText)
		if partText == "" {
			continue
		}
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		content.WriteString(partText)
	}

	finalText := strings.TrimSpace(content.String())
	if finalText == "" {
		return "", fmt.Errorf("no text content extracted from DOCX")
	}
	return finalText, nil
}

func isDOCXTextPart(name string) bool {
	if name == "word/document.xml" ||
		name == "word/footnotes.xml" ||
		name == "word/endnotes.xml" ||
		name == "word/comments.xml" {
		return true
	}
	if strings.HasPrefix(name, "word/header") && strings.HasSuffix(name, ".xml") {
		return true
	}
	if strings.HasPrefix(name, "word/footer") && strings.HasSuffix(name, ".xml") {
		return true
	}
	return false
}

func extractDOCXText(r io.Reader) (string, error) {
	decoder := xml.NewDecoder(r)
	var builder strings.Builder
	lastWasNewline := true

	appendNewline := func() {
		if !lastWasNewline {
			builder.WriteString("\n")
			lastWasNewline = true
		}
	}

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "t":
				var text string
				if err := decoder.DecodeElement(&text, &t); err != nil {
					return "", err
				}
				if text != "" {
					builder.WriteString(text)
					lastWasNewline = false
				}
			case "tab":
				builder.WriteString("\t")
				lastWasNewline = false
			case "br", "cr":
				appendNewline()
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "p" {
				appendNewline()
			}
		}
	}

	return strings.TrimSpace(builder.String()), nil
}

// ReadPPTXContent extracts text from a PPTX file.
// PPTX is a ZIP archive; slide text lives in ppt/slides/slide*.xml as <a:t> elements.
func ReadPPTXContent(path string) (string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("failed to open PPTX: %w", err)
	}
	defer reader.Close()

	var parts []string
	for _, file := range reader.File {
		name := strings.ToLower(file.Name)
		if strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml") {
			parts = append(parts, file.Name)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no slide XML parts found in PPTX")
	}
	sort.Strings(parts)

	var content strings.Builder
	for _, partName := range parts {
		var partFile *zip.File
		for _, file := range reader.File {
			if file.Name == partName {
				partFile = file
				break
			}
		}
		if partFile == nil {
			continue
		}

		rc, err := partFile.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open PPTX part %s: %w", partName, err)
		}
		slideText, parseErr := extractPPTXSlideText(rc)
		closeErr := rc.Close()
		if parseErr != nil {
			return "", fmt.Errorf("failed to parse PPTX part %s: %w", partName, parseErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("failed to close PPTX part %s: %w", partName, closeErr)
		}

		slideText = strings.TrimSpace(slideText)
		if slideText == "" {
			continue
		}
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		content.WriteString(slideText)
	}

	finalText := strings.TrimSpace(content.String())
	if finalText == "" {
		return "", fmt.Errorf("no text content extracted from PPTX")
	}
	return finalText, nil
}

func extractPPTXSlideText(r io.Reader) (string, error) {
	decoder := xml.NewDecoder(r)
	var builder strings.Builder
	lastWasNewline := true

	appendNewline := func() {
		if !lastWasNewline {
			builder.WriteString("\n")
			lastWasNewline = true
		}
	}

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if strings.ToLower(t.Name.Local) == "t" {
				var text string
				if err := decoder.DecodeElement(&text, &t); err != nil {
					return "", err
				}
				if text != "" {
					builder.WriteString(text)
					lastWasNewline = false
				}
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "p" {
				appendNewline()
			}
		}
	}

	return strings.TrimSpace(builder.String()), nil
}

// ReadDOCContent extracts text from a legacy DOC file using external converters.
func ReadDOCContent(path string) (string, error) {
	converters := []struct {
		name string
		args []string
	}{
		{name: "textutil", args: []string{"-convert", "txt", "-stdout", path}},
		{name: "antiword", args: []string{path}},
	}

	var failures []string
	for _, c := range converters {
		output, err := exec.Command(c.name, c.args...).CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s failed: %v", c.name, err))
			continue
		}
		text := strings.TrimSpace(string(output))
		if text == "" {
			failures = append(failures, fmt.Sprintf("%s produced empty output", c.name))
			continue
		}
		return text, nil
	}

	return "", fmt.Errorf("failed to extract text from DOC: %s", strings.Join(failures, "; "))
}

// ReadPCAPContent summarizes a PCAP/PCAPNG capture (protocols, top talkers, sample packets).
func ReadPCAPContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open PCAP file: %w", err)
	}
	defer f.Close()

	var packetSource *gopacket.PacketSource
	if strings.ToLower(filepath.Ext(path)) == ".pcapng" {
		ngReader, err := pcapgo.NewNgReader(f, pcapgo.DefaultNgReaderOptions)
		if err != nil {
			return "", fmt.Errorf("failed to create PCAPNG reader: %w", err)
		}
		packetSource = gopacket.NewPacketSource(ngReader, ngReader.LinkType())
	} else {
		reader, err := pcapgo.NewReader(f)
		if err != nil {
			return "", fmt.Errorf("failed to create PCAP reader: %w", err)
		}
		packetSource = gopacket.NewPacketSource(reader, reader.LinkType())
	}

	var builder strings.Builder
	builder.WriteString("=== PCAP File Analysis ===\n\n")
	return buildPCAPSummary(&builder, packetSource)
}

func buildPCAPSummary(builder *strings.Builder, packetSource *gopacket.PacketSource) (string, error) {
	totalPackets := 0
	protocolCount := make(map[string]int)
	srcIPs := make(map[string]int)
	dstIPs := make(map[string]int)
	srcPorts := make(map[string]int)
	dstPorts := make(map[string]int)
	var firstTimestamp, lastTimestamp string

	const maxDetailedPackets = 10
	var detailedPackets []string

	for packet := range packetSource.Packets() {
		totalPackets++

		if totalPackets == 1 {
			firstTimestamp = packet.Metadata().Timestamp.String()
		}
		lastTimestamp = packet.Metadata().Timestamp.String()

		if networkLayer := packet.NetworkLayer(); networkLayer != nil {
			if ipLayer := packet.Layer(layers.LayerTypeIPv4); ipLayer != nil {
				ip, _ := ipLayer.(*layers.IPv4)
				srcIPs[ip.SrcIP.String()]++
				dstIPs[ip.DstIP.String()]++
				protocolCount["IPv4"]++
			} else if ipLayer := packet.Layer(layers.LayerTypeIPv6); ipLayer != nil {
				ip, _ := ipLayer.(*layers.IPv6)
				srcIPs[ip.SrcIP.String()]++
				dstIPs[ip.DstIP.String()]++
				protocolCount["IPv6"]++
			}
		}

		if transportLayer := packet.TransportLayer(); transportLayer != nil {
			switch transportLayer.LayerType() {
			case layers.LayerTypeTCP:
				tcp, _ := transportLayer.(*layers.TCP)
				srcPorts[fmt.Sprintf("%d", tcp.SrcPort)]++
				dstPorts[fmt.Sprintf("%d", tcp.DstPort)]++
				protocolCount["TCP"]++
			case layers.LayerTypeUDP:
				udp, _ := transportLayer.(*layers.UDP)
				srcPorts[fmt.Sprintf("%d", udp.SrcPort)]++
				dstPorts[fmt.Sprintf("%d", udp.DstPort)]++
				protocolCount["UDP"]++
			}
		}

		if packet.ApplicationLayer() != nil {
			if packet.Layer(layers.LayerTypeDNS) != nil {
				protocolCount["DNS"]++
			} else if packet.Layer(layers.LayerTypeTLS) != nil {
				protocolCount["TLS"]++
			} else if tcpLayer := packet.Layer(layers.LayerTypeTCP); tcpLayer != nil {
				tcp, _ := tcpLayer.(*layers.TCP)
				if tcp.DstPort == 80 || tcp.SrcPort == 80 || tcp.DstPort == 8080 || tcp.SrcPort == 8080 {
					protocolCount["HTTP"]++
				}
			}
		}

		if totalPackets <= maxDetailedPackets {
			detailedPackets = append(detailedPackets, fmt.Sprintf("Packet #%d: %s", totalPackets, packet.String()))
		}

		if totalPackets >= 100000 {
			builder.WriteString("Large capture detected. Processing first 100,000 packets only.\n\n")
			break
		}
	}

	builder.WriteString("Summary:\n")
	builder.WriteString(fmt.Sprintf("- Total Packets: %d\n", totalPackets))
	builder.WriteString(fmt.Sprintf("- First Packet: %s\n", firstTimestamp))
	builder.WriteString(fmt.Sprintf("- Last Packet: %s\n\n", lastTimestamp))

	builder.WriteString("Protocols:\n")
	for proto, count := range protocolCount {
		percentage := float64(count) / float64(totalPackets) * 100
		builder.WriteString(fmt.Sprintf("- %s: %d packets (%.2f%%)\n", proto, count, percentage))
	}
	builder.WriteString("\n")

	topCount := 5
	builder.WriteString("Top Source IPs:\n")
	for ip, count := range topN(srcIPs, topCount) {
		builder.WriteString(fmt.Sprintf("- %s: %d packets\n", ip, count))
	}
	builder.WriteString("\n")

	builder.WriteString("Top Destination IPs:\n")
	for ip, count := range topN(dstIPs, topCount) {
		builder.WriteString(fmt.Sprintf("- %s: %d packets\n", ip, count))
	}
	builder.WriteString("\n")

	builder.WriteString("Top Source Ports:\n")
	for port, count := range topN(srcPorts, topCount) {
		builder.WriteString(fmt.Sprintf("- Port %s: %d packets\n", port, count))
	}
	builder.WriteString("\n")

	builder.WriteString("Top Destination Ports:\n")
	for port, count := range topN(dstPorts, topCount) {
		builder.WriteString(fmt.Sprintf("- Port %s: %d packets\n", port, count))
	}
	builder.WriteString("\n")

	if len(detailedPackets) > 0 {
		builder.WriteString("Sample Packets (first 3):\n")
		for i, pkt := range detailedPackets {
			if i >= 3 {
				break
			}
			builder.WriteString(fmt.Sprintf("\n%s\n", pkt))
		}
	}

	return builder.String(), nil
}

func topN(m map[string]int, n int) map[string]int {
	type kv struct {
		key   string
		value int
	}

	var sorted []kv
	for k, v := range m {
		sorted = append(sorted, kv{k, v})
	}

	for i := 0; i < len(sorted) && i < n; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].value > sorted[i].value {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	result := make(map[string]int)
	for i := 0; i < len(sorted) && i < n; i++ {
		result[sorted[i].key] = sorted[i].value
	}
	return result
}
