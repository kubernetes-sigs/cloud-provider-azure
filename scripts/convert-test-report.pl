#!/usr/bin/perl

# Copyright 2018 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

use 5.012;

say (reporter->new()
      ->appendLogFile(shift,
        qr/^--- (?<pass>FAIL|PASS):\s+(?<name>.*?)\s+\((?<time>[\d\.]+)s\)/,
        'PASS',
        qr/^(?<pass>FAIL|ok)\s+(?<name>.*?)\s+(?<time>[\d\.]+)s/)
      ->generateReport());

package reporter;
sub new {
  bless {
    total    => 0,
    failures => 0,
    time     => 0,
    report   => '',
  }
}

sub generateReport {
  my $d1 = shift;
<<EOM;
<?xml version="1.0" encoding="UTF-8"?>
<testsuite failures="$d1->{failures}" tests="$d1->{total}" time="$d1->{time}">
$d1->{report}</testsuite>
EOM
}

sub appendLogFile {
  my ($d1, $file, $case_pattern, $case_ok, $class_pattern) = @_;
  open(my $fl, '<', $file);
  my $buffer = '';
  my $pout = \$buffer;
  my @cases;
  
  my $case = ();
  while(<$fl>) {
    next if /^(PASS|FAIL)$/;
    $pout = \$buffer if (/^=== RUN/);
    s/\t/ /g,s/[^[:ascii:]]//g,s/[^[:print:]]//g; # for xml output
    if (/$class_pattern/) {
      my $class = $+{name} =~ s#.*/pkg/##r;
      for my $case (@cases) {
        $d1->{time} += $case->{time};
        $d1->{total}++;
        $d1->{failures}++ unless $case->{pass};
        $d1->{report} .= _generate_section($case, $class);
      }
      splice @cases;
      next;
    }
    $$pout .= $_."\n";
    next unless /$case_pattern/;
    $case = {
      name    => $+{name},
      time    => $+{time} // 0,
      pass    => $+{pass} eq $case_ok,
      sysout  => $buffer,
    };
    $pout = \$case->{sysout};
    $buffer = '';

    push @cases, $case;
  }
  close($fl);

  $d1
}

sub _generate_section {
  my $entry = shift;
  my $class = shift || '(default)';
  my $xml = <<EOM;
<testcase classname="$class" name="$entry->{name}" time="$entry->{time}">
<system-out><![CDATA[$entry->{sysout}]]></system-out>
EOM
  $xml .= '<failure type="Failure" />'."\n" unless $entry->{pass};
  $xml .= "</testcase>\n";
  return $xml;
}
