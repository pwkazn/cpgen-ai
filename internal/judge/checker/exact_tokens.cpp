// CPGen exact-tokens-v1. Standard C++20; testlib-compatible exit codes.
// The whitespace set matches Go bytes.Fields / Unicode White_Space.
#include <fstream>
#include <string>
#include <string_view>

static constexpr std::size_t max_bytes = 1u << 20;

static bool read_bounded(const char* path, std::string& result) {
    std::ifstream file(path, std::ios::binary);
    if (!file) return false;
    char buffer[8192];
    while (file.read(buffer, sizeof(buffer)) || file.gcount()) {
        auto count = static_cast<std::size_t>(file.gcount());
        if (count > max_bytes - result.size()) return false;
        result.append(buffer, count);
    }
    return file.eof() && !file.bad();
}

// Only complete UTF-8 encodings of whitespace are skipped. All other bytes,
// including invalid/truncated UTF-8 and NUL, remain exact token content.
static std::size_t space_width(const std::string& s, std::size_t i) {
    auto a = static_cast<unsigned char>(s[i]);
    if (a == 0x20 || (a >= 0x09 && a <= 0x0d)) return 1;
    if (i + 1 < s.size()) {
        auto b = static_cast<unsigned char>(s[i + 1]);
        if (a == 0xc2 && (b == 0x85 || b == 0xa0)) return 2;
        if (i + 2 < s.size()) {
            auto c = static_cast<unsigned char>(s[i + 2]);
            if ((a == 0xe1 && b == 0x9a && c == 0x80) ||
                (a == 0xe2 && b == 0x80 && ((c >= 0x80 && c <= 0x8a) || c == 0xa8 || c == 0xa9 || c == 0xaf)) ||
                (a == 0xe2 && b == 0x81 && c == 0x9f) ||
                (a == 0xe3 && b == 0x80 && c == 0x80)) return 3;
        }
    }
    return 0;
}

static bool next_token(const std::string& s, std::size_t& cursor, std::string_view& token) {
    while (cursor < s.size()) {
        auto width = space_width(s, cursor);
        if (!width) break;
        cursor += width;
    }
    if (cursor == s.size()) return false;
    auto start = cursor;
    while (cursor < s.size() && !space_width(s, cursor)) ++cursor;
    token = std::string_view(s).substr(start, cursor - start);
    return true;
}

int main(int argc, char** argv) {
    if (argc != 1 && argc != 4) return 3;
    const char* input_path = argc == 4 ? argv[1] : "/input/input.txt";
    const char* output_path = argc == 4 ? argv[2] : "/input/output.txt";
    const char* answer_path = argc == 4 ? argv[3] : "/input/answer.txt";
    std::ifstream input(input_path, std::ios::binary);
    if (!input) return 3;
    std::string output, answer;
    if (!read_bounded(answer_path, answer) || !read_bounded(output_path, output)) return 3;
    std::size_t left = 0, right = 0;
    std::string_view candidate, expected;
    for (;;) {
        bool has_candidate = next_token(output, left, candidate);
        bool has_expected = next_token(answer, right, expected);
        if (has_candidate != has_expected) return 1;
        if (!has_candidate) return 0;
        if (candidate != expected) return 1;
    }
}
