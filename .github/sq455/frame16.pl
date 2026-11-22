# Usage: perl frame16.pl file.go
# Adds a 16-byte array to the frame of every function that calls b.ReportAllocs,
# live to the end of the function by a deferred write.
local $/;
my $file = shift;
open(my $in, '<', $file) or die "$file: $!";
my $src = <$in>;
close($in);
my $n = 0;
$src =~ s{(\n(\t+)b\.ReportAllocs\(\)\n)}{
	$n++;
	"$1$2var sq466pad${n} [16]byte\n$2defer func() { sq466pad${n}[0]++ }()\n"
}ge;
open(my $out, '>', $file) or die "$file: $!";
print $out $src;
close($out);
