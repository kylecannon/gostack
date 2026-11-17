package main

import (
	"net/http"
)

// buildExceptions reads the error shapes of the primary model. For a service
// that is awsQueryCompatible, the Code is the shape name and the QueryCode is
// the code the query model gives the same shape, which is what the json
// protocol reports in x-amzn-query-error and the query protocol in <Code>.
func (g *generator) buildExceptions() error {
	compatible := g.p.Metadata.IsQueryCompatible()

	for _, name := range sortedKeys(g.p.Shapes) {
		shape := g.p.Shapes[name]
		if !shape.Exception {
			continue
		}

		ex := exception{shape: name, code: name, status: http.StatusBadRequest}

		if trait := shape.Error; trait != nil {
			if trait.Code != "" && !compatible {
				ex.code = trait.Code
			}

			if trait.HTTPStatusCode != 0 {
				ex.status = trait.HTTPStatusCode
			}

			ex.sender = trait.SenderFault
		}

		if compatible && g.q != nil {
			ex.queryCode = name

			if qs := g.q.Shapes[name]; qs != nil && qs.Error != nil {
				if qs.Error.Code != "" {
					ex.queryCode = qs.Error.Code
				}

				ex.sender = ex.sender || qs.Error.SenderFault
			}
		}

		g.exceptions = append(g.exceptions, ex)
	}

	return nil
}

// errorsFile writes one constructor for each error shape.
func (g *generator) errorsFile() (string, error) {
	p := &printer{}

	for _, ex := range g.exceptions {
		fault := "awserr.FaultReceiver"
		if ex.sender {
			fault = "awserr.FaultSender"
		}

		p.f("// New%s returns the %s error with the given message.", exportName(ex.shape), ex.shape)
		p.f("func New%s(msg string) *awserr.Error {", exportName(ex.shape))
		p.f("\treturn &awserr.Error{")
		p.f("\t\tCode: %q,", ex.code)
		p.f("\t\tMessage: msg,")
		p.f("\t\tStatusCode: %d,", ex.status)
		p.f("\t\tFault: %s,", fault)

		if ex.queryCode != "" {
			p.f("\t\tQueryCode: %q,", ex.queryCode)
		}

		p.f("\t}")
		p.f("}")
		p.blank()
	}

	g.missingRequiredFunc(p)

	return p.buf.String(), nil
}

// missingRequiredFunc writes the error for a required member the request
// lacks, in LocalStack's message.
func (g *generator) missingRequiredFunc(p *printer) {
	code, queryCode := "ValidationError", ""

	if mp := g.opts.MissingParam; mp != nil {
		code, queryCode = mp.Code, mp.QueryCode
	}

	p.f("// newMissingRequired returns the error for a request that lacks a required member.")
	p.f("func newMissingRequired(member string) *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: %q,", code)
	p.f("\t\tMessage: \"The request must contain the parameter \" + member + \".\",")
	p.f("\t\tStatusCode: 400,")
	p.f("\t\tFault: awserr.FaultSender,")

	if queryCode != "" {
		p.f("\t\tQueryCode: %q,", queryCode)
	}

	p.f("\t}")
	p.f("}")
}
