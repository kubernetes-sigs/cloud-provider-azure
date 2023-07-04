// /*
// Copyright 2023 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

%{
package provider

import (
    "fmt"
	"unicode/utf8"
    "strings"
)
%}

%union {
   DestinationPrefixes map[string]map[string]map[string]struct{}
   NamespaceLists map[string]map[string]struct{}
   ServiceLists map[string]struct{}
   String string
}

%type <DestinationPrefixes> items 
%type <DestinationPrefixes> item  
%type <String> destinationPrefix dnsName alphabet text 
%type <ServiceLists> svcNameList
%type <NamespaceLists> namespaceList namespaceLists

%token ',' ';' '&' 
%token <String> '0' '1' '2' '3' '4' '5' '6' '7' '8' '9' 'a' 'b' 'c' 'd' 'e' 'f' 'g' 'h' 'i' 'j' 'k' 'l' 'm' 'n' 'o' 'p' 'q' 'r' 's' 't' 'u' 'v' 'w' 'x' 'y' 'z' 'A' 'B' 'C' 'D' 'E' 'F' '-' '[' ']' '.' ':' '/'


%%

items: /* empty */
{
    NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes = nil
    $$ = NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes
}
| items item ';'
{
    NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes = make(map[string]map[string]map[string]struct{})
    if $1 != nil {
        for k, v := range $1 {
             NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes[k] = v
        }
    } 
    if $2 != nil {
        for k, v := range $2 {
             NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes[k] = v
        }
    }
    $$ = NSGSharedRulelex.(*SecurityGroupSharedRuleNameLexerImpl).DestinationPrefixes
}
;

item: /* empty */
{
    $$ = nil
}
|destinationPrefix ':' namespaceLists
{
    $$ = make(map[string]map[string]map[string]struct{})
    $$[strings.ToLower($1)] = $3
}
;

destinationPrefix: text '.' text '.' text '.' text '/' text
|  text':' text ':' text ':' text '/' text
;

namespaceLists: namespaceLists ',' namespaceList
{
    $$ = make(map[string]map[string]struct{}) 
    for k,v := range $1 {
        $$[k] = v
    }
    for k, v := range $3 {
        $$[k] = v
    }
}
| namespaceList
{
    $$ = make(map[string]map[string]struct{})
    for k, v := range $1 {
        $$[k] = v
    }
}   
;

namespaceList: dnsName '/' svcNameList
{
    $$ = make(map[string]map[string]struct{})
    $$[strings.ToLower($1)] = $3
}
|svcNameList
{
    $$ = make(map[string]map[string]struct{})
    $$[strings.ToLower("default")] = $1
}
;
svcNameList: dnsName
{   
    $$ = make(map[string]struct{})
    $$[strings.ToLower($1)] = struct{}{}
}
| svcNameList '&' dnsName
{
    $$ = make(map[string]struct{})
    for k, v := range $1 {
        $$[k] = v
    }
    $$[strings.ToLower($3)] = struct{}{}
}
;

dnsName: alphabet 
| alphabet text alphabet
;

text: /* empty */
{
    $$ = ""
}
| text alphabet
| text '-'
;

alphabet: 'a'
| 'b'
| 'c'
| 'd'
| 'e'
| 'f'
| 'g'
| 'h'
| 'i'
| 'j'
| 'k'
| 'l'
| 'm'
| 'n'
| 'o'
| 'p'
| 'q'
| 'r'
| 's' 
| 't' 
| 'u'
| 'v'
| 'w' 
| 'x'
| 'y' 
| 'z'
| '1'
| '2'
| '3'
| '4'
| '5'
| '6'
| '7'
| '8'
| '9'
| '0'
;

%%

// The parser expects the lexer to return 0 on EOF.  Give it a name
// for clarity.
const eof = 0

// The parser uses the type SecurityGroupSharedRuleNameLexerImpl as a lexer. It must provide
// the methods Lex(*<prefix>SymType) int and Error(string).
type SecurityGroupSharedRuleNameLexerImpl struct {
	line []byte
	peek rune
    DestinationPrefixes map[string]map[string]map[string]struct{}
    Errs []error
}

// The parser calls this method to get each new token. This
// implementation returns operators and NUM.
func (x *SecurityGroupSharedRuleNameLexerImpl) Lex(yylval *NSGSharedRuleSymType) int {
	for {
		c := x.next()
		switch c {
		case eof:
			return eof
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c', 'd', 'e' ,'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'A', 'B', 'C', 'D', 'E', 'F',':', '/', ',', ';', '&', '.', '-':
			return int(c)
        case ' ', '\t', '\n', '\r':
            // ignore whitespace
		default:
            x.Errs = append(x.Errs, fmt.Errorf("unexpected character %q", c))
		}
	}
}

// Return the next rune for the lexer.
func (x *SecurityGroupSharedRuleNameLexerImpl) next() rune {
	if x.peek != eof {
		r := x.peek
		x.peek = eof
		return r
	}
	if len(x.line) == 0 {
		return eof
	}
	c, size := utf8.DecodeRune(x.line)
	x.line = x.line[size:]
	if c == utf8.RuneError && size == 1 {
		return x.next()
	}
	return c
}

// The parser calls this method on a parse error.
func (x *SecurityGroupSharedRuleNameLexerImpl) Error(s string) {
    x.Errs = append(x.Errs, fmt.Errorf("syntax error: %s", s))
}
