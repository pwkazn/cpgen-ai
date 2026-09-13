//go:build fixture_source

#include <fstream>
#include <string>

enum class ReadResult { ok, malformed, trailing, missing };

static ReadResult read_single(const char* path, long long& value) {
    std::ifstream input(path);
    if (!input) return ReadResult::missing;
    if (!(input >> value)) return ReadResult::malformed;
    input >> std::ws;
    if (input.peek() != std::char_traits<char>::eof()) return ReadResult::trailing;
    return ReadResult::ok;
}

int main() {
    long long a, b, answer, output;
    std::ifstream test("/input/input.txt");
    if (!(test >> a >> b)) return 3;
    test >> std::ws;
    if (test.peek() != std::char_traits<char>::eof()) return 3;

    if (read_single("/input/answer.txt", answer) != ReadResult::ok) return 3;
    ReadResult candidate = read_single("/input/output.txt", output);
    if (candidate == ReadResult::missing || candidate == ReadResult::malformed) return 2;
    if (candidate == ReadResult::trailing) return 4;
    if (output != answer) return 1;
    return 0;
}
