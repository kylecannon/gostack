package s3

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/account"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/sigv4"
)

const (
	// maxPreData is what the form may hold before its file: S3's 20 KB of "form
	// data and boundaries (excluding the contents of the file)".
	maxPreData = 20 << 10
	// partOverhead is charged for each part besides its boundary, headers and
	// value: the CRLFs around them. With the boundary it is at least
	// partOverhead, so maxPreData also caps the part count.
	partOverhead = 8

	formFile           = "file"
	fieldAccessKeyV2   = "awsaccesskeyid"
	fieldSignatureV2   = "signature"
	fieldSignatureV4   = "x-amz-signature"
	fieldCredentialsV4 = "x-amz-credential" //nolint:gosec // G101: a form field name, not a credential.

	policyExpirationLayout = "2006-01-02T15:04:05Z"
	conditionFailed        = "Invalid according to Policy: Policy Condition failed: "
	conditionArity         = 3
)

var (
	signatureV4Fields = []string{fieldSignatureV4, "x-amz-algorithm", fieldCredentialsV4, "x-amz-date"}
	signatureV2Fields = []string{fieldSignatureV2, fieldAccessKeyV2}
)

// PostObject stores the file of a browser upload form (LocalStack's
// post_object). The fields before the file are read; the file is streamed into
// PutObject, with the policy's content-length-range checked at its end. Field
// names are case-insensitive, and fields after the file are ignored, as S3
// does.
func (p *Provider) PostObject(ctx context.Context, in *s3api.PostObjectInput) (*s3api.PostObjectOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	contentType := rc.Request.Header.Get("Content-Type")
	if !strings.Contains(contentType, "multipart/form-data") {
		return nil, (&s3api.PreconditionFailed{
			Condition: new("Bucket POST must be of the enclosure-type multipart/form-data"),
		}).Err("At least one of the pre-conditions you specified did not hold")
	}

	bucket := deref(in.Bucket)

	_, err = p.bucket(rc.AccountID, bucket)
	if err != nil {
		return nil, err
	}

	_, params, _ := mime.ParseMediaType(contentType)

	form, file, err := readForm(in.Body, params["boundary"])
	if err != nil {
		return nil, err
	}

	ranges, err := checkPolicy(form, bucket)
	if err != nil {
		return nil, firstFailure(ranges, file, err)
	}

	if file == nil {
		return nil, (&s3api.InvalidArgument{
			ArgumentName: new(formFile), ArgumentValue: new("0"), HostId: new(hostID),
		}).Err("POST requires exactly one file upload per request.")
	}

	key := form["key"]
	if name := file.FileName(); name != "" {
		key = strings.ReplaceAll(key, "${filename}", name)
	}

	put, err := putInput(form, bucket, key)
	if err != nil {
		return nil, err
	}

	put.Body = file
	if len(ranges) > 0 {
		put.Body = &lengthChecked{r: file, ranges: ranges}
	}

	out, err := p.PutObject(ctx, put)
	if err != nil {
		return nil, err
	}

	return p.postOutput(form, bucket, key, out), nil
}

// readForm reads the fields of a multipart form up to its file part, which it
// returns unread; nil when the form has none. Names are in lower case, and a
// name's first value wins. Each field is charged its boundary, its headers and
// its value against maxPreData, so many empty fields are refused as a few long
// ones are.
func readForm(body io.Reader, boundary string) (map[string]string, *multipart.Part, error) {
	if boundary == "" || body == nil {
		return nil, nil, malformedPost()
	}

	reader := multipart.NewReader(body, boundary)
	form := make(map[string]string)
	budget := int64(maxPreData)

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return form, nil, nil
		}

		if err != nil {
			return nil, nil, malformedPost()
		}

		name := strings.ToLower(part.FormName())
		if name == formFile {
			return form, part, nil
		}

		budget -= int64(partOverhead + len(boundary) + headerSize(part.Header))
		if budget < 0 {
			return nil, nil, preDataTooLarge()
		}

		value, err := io.ReadAll(io.LimitReader(part, budget+1))
		if err != nil {
			return nil, nil, malformedPost()
		}

		budget -= int64(len(value))
		if budget < 0 {
			return nil, nil, preDataTooLarge()
		}

		if _, seen := form[name]; !seen {
			form[name] = string(value)
		}
	}
}

// headerSize is the bytes of a part's header lines: name, ": ", value, CRLF.
func headerSize(header textproto.MIMEHeader) int {
	size := 0

	for name, values := range header {
		for _, value := range values {
			size += len(name) + len(value) + len(": \r\n")
		}
	}

	return size
}

func preDataTooLarge() *awserr.Error {
	return configError("MaxPostPreDataLengthExceeded",
		"Your POST request fields preceding the upload file were too large.", http.StatusBadRequest)
}

func malformedPost() *awserr.Error {
	return configError("MalformedPOSTRequest",
		"The body of your POST request is not well-formed multipart/form-data.", http.StatusBadRequest)
}

// lengthRange is a policy's content-length-range condition.
type lengthRange struct {
	low  int64
	high int64
}

// checkPolicy is validate_post_policy without the file's size: the key, the
// signature fields, the policy's expiry and its conditions in order. A
// content-length-range condition is returned rather than checked; the ones
// before the first failing condition come back with its error.
func checkPolicy(form map[string]string, bucket string) ([]lengthRange, error) {
	if form["key"] == "" {
		return nil, missingField("key")
	}

	document, err := readPolicy(form)
	if err != nil || document == nil {
		return nil, err
	}

	err = checkExpiration(document["expiration"])
	if err != nil {
		return nil, err
	}

	var (
		conditions []json.RawMessage
		ranges     []lengthRange
	)

	_ = json.Unmarshal(document["conditions"], &conditions)

	for _, condition := range conditions {
		bounds, met, err := checkCondition(condition, form, bucket)
		if err != nil {
			return ranges, err
		}

		if !met {
			return ranges, (&s3api.AccessDenied{HostId: new(hostID)}).Err(conditionFailed + conditionText(condition))
		}

		if bounds != nil {
			ranges = append(ranges, *bounds)
		}
	}

	return ranges, nil
}

// readPolicy returns the form's policy document, nil when it has none, after
// checking its signature fields. A policy that does not decode is a signature
// that does not match.
func readPolicy(form map[string]string) (map[string]json.RawMessage, error) {
	policy := form["policy"]
	if policy == "" {
		// A bucket that anyone can write to needs no policy.
		return nil, nil //nolint:nilnil // No policy is not an error, and there is no document.
	}

	isV4, err := signatureFields(form, signatureV4Fields)
	if err != nil {
		return nil, err
	}

	isV2, err := signatureFields(form, signatureV2Fields)
	if err != nil {
		return nil, err
	}

	if !isV4 && !isV2 {
		return nil, (&s3api.AccessDenied{HostId: new(hostID)}).Err("Access Denied")
	}

	document, ok := decodePolicy(policy)
	if ok {
		return document, nil
	}

	signature := form[fieldSignatureV4]
	if isV2 {
		signature = form[fieldSignatureV2]
	}

	// Whatever the credential's shape, its first field is the access key.
	scope, _ := account.ParseScope(form[fieldCredentialsV4])
	credential := scope.AccessKey

	return nil, (&s3api.SignatureDoesNotMatch{
		AWSAccessKeyId:    new(cmp.Or(form[fieldAccessKeyV2], credential, presignAccessKey)),
		HostId:            new(hostID),
		SignatureProvided: new(signature),
		StringToSign:      new(policy),
		StringToSignBytes: new(hexBytes(policy)),
	}).Err(signatureMismatch)
}

// signatureFields reports whether the form has one of fields, and refuses it
// when it has some but not all.
func signatureFields(form map[string]string, fields []string) (bool, error) {
	if !slices.ContainsFunc(fields, func(field string) bool { _, ok := form[field]; return ok }) {
		return false, nil
	}

	for _, field := range fields {
		if _, ok := form[field]; !ok {
			return false, missingField(fieldName(field))
		}
	}

	return true, nil
}

// fieldName is how a missing signature field is named: AWSAccessKeyId, or
// each word capitalized.
func fieldName(field string) string {
	if field == fieldAccessKeyV2 {
		return paramAccessKeyV2
	}

	words := strings.Split(field, "-")
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}

	return strings.Join(words, "-")
}

func missingField(name string) error {
	return (&s3api.InvalidArgument{ArgumentName: new(name), ArgumentValue: new(""), HostId: new(hostID)}).Err(
		"Bucket POST must contain a field named '" + name + "'.  If it is specified, please check the order of " +
			"the fields.")
}

// decodePolicy reads a policy document: base64, which skips characters
// outside its alphabet as Python's b64decode does, of a JSON object in UTF-8.
func decodePolicy(policy string) (map[string]json.RawMessage, bool) {
	kept := strings.Map(func(r rune) rune {
		if ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') ||
			r == '+' || r == '/' || r == '=' {
			return r
		}

		return -1
	}, policy)

	raw, err := base64.StdEncoding.DecodeString(kept)
	if err != nil || !utf8.Valid(raw) {
		return nil, false
	}

	var document map[string]json.RawMessage

	err = json.Unmarshal(raw, &document)

	return document, err == nil && document != nil
}

// checkExpiration refuses a policy whose expiration has passed.
func checkExpiration(raw json.RawMessage) error {
	var expiration any

	_ = json.Unmarshal(raw, &expiration)

	switch v := expiration.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
	case bool:
		if !v {
			return nil
		}
	default:
	}

	text, _ := expiration.(string)

	expiresAt, err := time.Parse(policyExpirationLayout, text)
	if err != nil {
		return invalidPolicy("Invalid Policy: Invalid 'expiration' value: '" + strings.Trim(string(raw), `"`) + "'")
	}

	if time.Now().After(expiresAt) {
		return (&s3api.AccessDenied{HostId: new(hostID)}).Err("Invalid according to Policy: Policy expired.")
	}

	return nil
}

func invalidPolicy(message string) *awserr.Error {
	return configError("InvalidPolicyDocument", message, http.StatusBadRequest)
}

// checkCondition is _verify_condition for every condition but the size: a
// one-member object, or an eq, starts-with or content-length-range list. A
// content-length-range condition returns its bounds.
func checkCondition(raw json.RawMessage, form map[string]string, bucket string) (*lengthRange, bool, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		if len(object) > 1 {
			return nil, false, invalidPolicy("Invalid Policy: Invalid Simple-Condition: " +
				"Simple-Conditions must have exactly one property specified.")
		}

		for name, value := range object {
			lower := strings.ToLower(name)
			if lower == "bucket" {
				return nil, jsonString(value, bucket, true), nil
			}

			field, ok := form[lower]

			return nil, jsonString(value, field, ok), nil
		}

		return nil, false, nil
	}

	list := conditionList(raw)
	if list == nil {
		return nil, false, nil
	}

	var operator, name string

	_ = json.Unmarshal(list[0], &operator)
	nameIsText := json.Unmarshal(list[1], &name) == nil

	switch {
	case operator == "eq" && nameIsText:
		lower := strings.ToLower(name)
		if lower == "$bucket" {
			return nil, jsonString(list[2], bucket, true), nil
		}

		field, ok := form[strings.TrimLeft(lower, "$")]

		return nil, strings.HasPrefix(lower, "$") && jsonString(list[2], field, ok), nil
	case operator == "starts-with" && nameIsText:
		var prefix any

		_ = json.Unmarshal(list[2], &prefix)
		text, _ := prefix.(string)
		empty := prefix == nil || text == "" && !isNonEmptyValue(prefix)

		return nil, strings.HasPrefix(name, "$") &&
			(empty || strings.HasPrefix(form[strings.ToLower(strings.TrimLeft(name, "$"))], text)), nil
	case operator == "content-length-range":
		low, lowOK := pythonInt(list[1])
		high, highOK := pythonInt(list[2])

		return &lengthRange{low: low, high: high}, lowOK && highOK, nil
	default:
		return nil, false, nil
	}
}

// conditionList is a condition that is a list of three, or nil.
func conditionList(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage

	_ = json.Unmarshal(raw, &list)

	if len(list) != conditionArity {
		return nil
	}

	return list
}

// jsonString reports whether a JSON value equals a form value, as Python's
// == does: only a string can, and null equals a missing one.
func jsonString(raw json.RawMessage, value string, present bool) bool {
	var decoded any

	_ = json.Unmarshal(raw, &decoded)

	if decoded == nil {
		return !present
	}

	text, ok := decoded.(string)

	return ok && present && text == value
}

// isNonEmptyValue reports whether a starts-with value that is not a string is
// true to Python: a non-zero number, true, or a non-empty list or object.
func isNonEmptyValue(value any) bool {
	switch v := value.(type) {
	case float64:
		return v != 0
	case bool:
		return v
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return false
	}
}

// pythonInt is int() of a JSON number or string.
func pythonInt(raw json.RawMessage) (int64, bool) {
	var value any

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	if decoder.Decode(&value) != nil {
		return 0, false
	}

	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n, true
		}

		f, err := v.Float64()

		return int64(f), err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)

		return n, err == nil
	default:
		return 0, false
	}
}

// firstFailure is the error of a policy condition, unless a content-length-range
// before it fails, which LocalStack checks in order. The file is read to its
// end to learn its size.
func firstFailure(ranges []lengthRange, file io.Reader, err error) error {
	if len(ranges) == 0 || file == nil {
		return err
	}

	size, copyErr := io.Copy(io.Discard, file)
	if copyErr != nil {
		return malformedPost()
	}

	sizeErr := checkLength(ranges, size)
	if sizeErr != nil {
		return sizeErr
	}

	return err
}

// checkLength is the content-length-range check of a file of size bytes.
func checkLength(ranges []lengthRange, size int64) error {
	for _, bounds := range ranges {
		switch {
		case size < bounds.low:
			return (&s3api.EntityTooSmall{ProposedSize: new(size), MinSizeAllowed: new(bounds.low)}).Err(
				"Your proposed upload is smaller than the minimum allowed size")
		case size > bounds.high:
			return (&s3api.EntityTooLarge{
				ProposedSize: new(size), MaxSizeAllowed: new(strconv.FormatInt(bounds.high, 10)), HostId: new(hostID),
			}).Err("Your proposed upload exceeds the maximum allowed size")
		default:
		}
	}

	return nil
}

// lengthChecked counts the file as PutObject reads it and fails the read that
// ends it when the size is out of the policy's range, so the blob is dropped.
type lengthChecked struct {
	r      io.Reader
	ranges []lengthRange
	size   int64
}

func (l *lengthChecked) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.size += int64(n)

	if errors.Is(err, io.EOF) {
		tooBig := checkLength(l.ranges, l.size)
		if tooBig != nil {
			return n, tooBig
		}
	}

	return n, err
}

// putInput is the PutObject a form makes, without its body.
func putInput(form map[string]string, bucket, key string) (*s3api.PutObjectInput, error) {
	if class, ok := form["x-amz-storage-class"]; ok {
		err := checkStorageClass(&class)
		if err != nil {
			return nil, err
		}
	}

	tagging, err := taggingOf(form["tagging"])
	if err != nil {
		return nil, err
	}

	in := &s3api.PutObjectInput{
		Bucket:                  new(bucket),
		Key:                     new(key),
		ACL:                     nonEmpty(form["acl"]),
		CacheControl:            nonEmpty(latin1(form["cache-control"])),
		ContentType:             nonEmpty(latin1(form["content-type"])),
		ContentDisposition:      nonEmpty(latin1(form["content-disposition"])),
		ContentEncoding:         nonEmpty(latin1(form["content-encoding"])),
		Expires:                 nonEmpty(form["expires"]),
		StorageClass:            nonEmpty(form["x-amz-storage-class"]),
		ServerSideEncryption:    nonEmpty(form["x-amz-server-side-encryption"]),
		SSEKMSKeyId:             nonEmpty(form["x-amz-server-side-encryption-aws-kms-key-id"]),
		WebsiteRedirectLocation: nonEmpty(form["x-amz-website-redirect-location"]),
		ChecksumAlgorithm:       nonEmpty(strings.ToUpper(form["x-amz-checksum-algorithm"])),
		Tagging:                 nonEmpty(tagging),
	}

	for name, value := range form {
		if metaKey, ok := strings.CutPrefix(name, "x-amz-meta-"); ok {
			if in.Metadata == nil {
				in.Metadata = make(map[string]string)
			}

			in.Metadata[metaKey] = asSent(value)
		}
	}

	algorithm := deref(in.ChecksumAlgorithm)
	value := nonEmpty(form["x-amz-checksum-"+strings.ToLower(algorithm)])

	switch algorithm {
	case algorithmCRC32:
		in.ChecksumCRC32 = value
	case algorithmCRC32C:
		in.ChecksumCRC32C = value
	case algorithmCRC64NVME:
		in.ChecksumCRC64NVME = value
	case algorithmSHA1:
		in.ChecksumSHA1 = value
	case algorithmSHA256:
		in.ChecksumSHA256 = value
	default:
	}

	return in, nil
}

// latin1 is a form field as S3 sends it back in a header: in ISO-8859-1, with
// each character outside it made a space (LocalStack's
// replace_non_iso_8859_1_characters). A header PutObject reads is bytes
// already, so only a form field needs this.
func latin1(value string) string {
	if !strings.ContainsFunc(value, func(r rune) bool { return r >= utf8.RuneSelf }) {
		return value
	}

	out := make([]byte, 0, len(value))

	for _, r := range value {
		if r > unicode.MaxLatin1 {
			r = ' '
		}

		out = append(out, byte(r))
	}

	return string(out)
}

// asSent is a metadata field that PutObject stores as it is. PutObject decodes
// the RFC 2047 words of a header; S3 keeps those of a form field, so a value
// that would decode is handed over as one base64 word of itself.
func asSent(value string) string {
	if decodeHeaderRFC2047(value) == value {
		return value
	}

	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

// postTagging is the Tagging document of a form.
type postTagging struct {
	Tags []postTag `xml:"TagSet>Tag"`
}

type postTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// taggingOf turns a form's Tagging XML into the x-amz-tagging header form, ""
// when it has no tags.
func taggingOf(document string) (string, error) {
	if document == "" {
		return "", nil
	}

	var tagging postTagging

	err := xml.Unmarshal([]byte(document), &tagging)
	if err != nil {
		return "", malformedXMLError()
	}

	pairs := make([]string, 0, len(tagging.Tags))
	for _, tag := range tagging.Tags {
		pairs = append(pairs, url.QueryEscape(tag.Key)+"="+url.QueryEscape(tag.Value))
	}

	return strings.Join(pairs, "&"), nil
}

// postOutput is the answer to a stored form: a 303 to success_action_redirect
// when it is a URL with a host, else success_action_status when it is 200 or
// 201, else 204. Only 201 has a body.
func (p *Provider) postOutput(
	form map[string]string, bucket, key string, put *s3api.PutObjectOutput,
) *s3api.PostObjectOutput {
	status := http.StatusNoContent

	code, err := strconv.Atoi(form["success_action_status"])
	if err == nil && (code == http.StatusOK || code == http.StatusCreated) {
		status = code
	}

	location := p.bucketURL(bucket) + sigv4.EscapePath(key)
	if redirect, ok := redirectTo(form["success_action_redirect"], bucket, key, deref(put.ETag)); ok {
		location, status = redirect, http.StatusSeeOther
	}

	out := &s3api.PostObjectOutput{
		ETagHeader:           put.ETag,
		LocationHeader:       new(location),
		StatusCode:           new(int32(status)),
		Expiration:           put.Expiration,
		VersionId:            put.VersionId,
		ServerSideEncryption: put.ServerSideEncryption,
		SSEKMSKeyId:          put.SSEKMSKeyId,
		BucketKeyEnabled:     put.BucketKeyEnabled,
		ChecksumCRC32:        put.ChecksumCRC32,
		ChecksumCRC32C:       put.ChecksumCRC32C,
		ChecksumCRC64NVME:    put.ChecksumCRC64NVME,
		ChecksumSHA1:         put.ChecksumSHA1,
		ChecksumSHA256:       put.ChecksumSHA256,
		ChecksumType:         put.ChecksumType,
	}

	if status == http.StatusCreated {
		out.Bucket, out.Key, out.ETag, out.Location = new(bucket), new(key), put.ETag, new(location)
	}

	return out
}

// redirectTo is create_redirect_for_post_request: base with key, bucket and
// etag in its query, which keeps its other non-empty parameters. A base with
// no host is no redirect.
func redirectTo(base, bucket, key, etag string) (string, bool) {
	parsed, err := url.Parse(base)
	if base == "" || err != nil || parsed.Host == "" {
		return "", false
	}

	var (
		names  []string
		values = make(map[string][]string)
	)

	for pair := range strings.SplitSeq(parsed.RawQuery, "&") {
		name, value, _ := strings.Cut(pair, "=")
		if value == "" {
			continue
		}

		name = unquotePlus(name)
		if values[name] == nil {
			names = append(names, name)
		}

		values[name] = append(values[name], unquotePlus(value))
	}

	for _, set := range [][2]string{{"key", key}, {"bucket", bucket}, {"etag", etag}} {
		if values[set[0]] == nil {
			names = append(names, set[0])
		}

		values[set[0]] = []string{set[1]}
	}

	var query strings.Builder

	for _, name := range names {
		for _, value := range values[name] {
			if query.Len() > 0 {
				query.WriteByte('&')
			}

			query.WriteString(url.QueryEscape(name) + "=" + url.QueryEscape(value))
		}
	}

	prefix, fragment := base, ""
	if i := strings.IndexByte(prefix, '#'); i >= 0 {
		prefix, fragment = prefix[:i], prefix[i:]
	}

	prefix, _, _ = strings.Cut(prefix, "?")

	return prefix + "?" + query.String() + fragment, true
}

// conditionText is a condition as Python's str() writes it, with ' made ",
// as LocalStack reports a failed one.
func conditionText(raw json.RawMessage) string {
	var value any

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)

	return strings.ReplaceAll(pythonRepr(value), "'", `"`)
}

func pythonRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}

		return "False"
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			return string(v)
		}

		f, _ := v.Float64()
		text := strconv.FormatFloat(f, 'g', -1, 64)

		if !strings.ContainsAny(text, ".eIN") {
			text += ".0"
		}

		return text
	case string:
		return pythonString(v)
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, pythonRepr(item))
		}

		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		items := make([]string, 0, len(v))
		for name, item := range v {
			items = append(items, pythonString(name)+": "+pythonRepr(item))
		}

		return "{" + strings.Join(items, ", ") + "}"
	default:
		return ""
	}
}

// pythonString is Python's repr of a str: single quotes, unless it holds one
// and no double quote.
func pythonString(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}

	var b strings.Builder

	b.WriteRune(quote)

	for _, r := range s {
		switch {
		case r == '\\' || r == quote:
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		default:
			b.WriteRune(r)
		}
	}

	b.WriteRune(quote)

	return b.String()
}
