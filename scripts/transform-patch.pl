#!/usr/bin/perl -w -n

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

state $skip = 0;
if (m#^diff --git a/(.*) .*$#) {
    my $file = $1;
    $skip = ($file !~ m#^pkg/cloudprovider/providers/azure/#) || ($file =~ /BUILD$/);
    print STDERR "# skipping file $file\n" if $skip;
}
next if $skip;

s#package azure\K#provider#;
print s#/cloudprovider/providers/azure/#/azureprovider/#gr;
